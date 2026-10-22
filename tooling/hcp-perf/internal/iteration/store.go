// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package iteration

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"syscall"

	_ "modernc.org/sqlite"
)

type Controller struct {
	root    string
	db      *sql.DB
	options Options
}

func Open(root string) (*Controller, error) {
	return NewController(root, Options{})
}

func NewController(root string, options Options) (*Controller, error) {
	if root == "" {
		return nil, errors.New("iteration root is required")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve iteration root: %w", err)
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, fmt.Errorf("create iteration root: %w", err)
	}
	info, err := os.Lstat(root)
	if err != nil {
		return nil, fmt.Errorf("inspect iteration root: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("iteration root must be a private directory (0700)")
	}
	if options.BinaryPath == "" {
		options.BinaryPath, err = os.Executable()
		if err != nil {
			return nil, fmt.Errorf("locate worker executable: %w", err)
		}
	}
	if options.ProcessFactory == nil {
		options.ProcessFactory = startWorker
	}
	if options.Executor == nil {
		options.Executor = libraryExecutor{client: options.HTTPClient}
	}
	path := filepath.Join(root, "iteration.sqlite")
	// Serialize first creation as well as version inspection across processes.
	schemaLock, err := os.OpenFile(filepath.Join(root, "schema.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("lock iteration schema: %w", err)
	}
	defer schemaLock.Close()
	if err := syscall.Flock(int(schemaLock.Fd()), syscall.LOCK_EX); err != nil {
		return nil, fmt.Errorf("lock iteration schema: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	created := err == nil
	if errors.Is(err, os.ErrExist) {
		f, err = os.OpenFile(path, os.O_RDWR, 0600)
	}
	if err != nil {
		return nil, fmt.Errorf("create iteration database: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	dsn := (&url.URL{Scheme: "file", Path: path}).String() + "?_pragma=synchronous(FULL)&_pragma=busy_timeout(10000)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open iteration database: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := initializeSchema(db, created); err != nil {
		db.Close()
		return nil, fmt.Errorf("validate iteration schema: %w", err)
	}
	_, err = db.Exec("PRAGMA journal_mode=WAL")
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize iteration schema: %w", err)
	}
	return &Controller{root: root, db: db, options: options}, nil
}

const schemaVersion = 2

var schemaTables = map[string]string{
	"runs":     "CREATE TABLE runs (id TEXT PRIMARY KEY, data BLOB NOT NULL)",
	"claims":   "CREATE TABLE claims (environment TEXT PRIMARY KEY, run_id TEXT NOT NULL UNIQUE)",
	"attempts": "CREATE TABLE attempts (token TEXT PRIMARY KEY, run_id TEXT NOT NULL)",
}

func initializeSchema(db *sql.DB, created bool) error {
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if !created {
		if version != schemaVersion {
			return fmt.Errorf("unsupported iteration database version %d (expected %d)", version, schemaVersion)
		}
		var tables int
		if err := db.QueryRow("SELECT count(*) FROM sqlite_schema WHERE type='table'").Scan(&tables); err != nil {
			return err
		}
		if tables != len(schemaTables) {
			return errors.New("iteration database schema is incomplete")
		}
		for name, expected := range schemaTables {
			var definition string
			if err := db.QueryRow("SELECT sql FROM sqlite_schema WHERE type='table' AND name=?", name).Scan(&definition); err != nil {
				return fmt.Errorf("inspect schema table %s: %w", name, err)
			}
			if definition != expected {
				return fmt.Errorf("unsupported schema definition for table %s", name)
			}
		}
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for name, definition := range schemaTables {
		if _, err := tx.Exec(definition); err != nil {
			return fmt.Errorf("create schema table %s: %w", name, err)
		}
	}
	if _, err := tx.Exec("PRAGMA user_version=2"); err != nil {
		return fmt.Errorf("set schema version: %w", err)
	}
	return tx.Commit()
}

func (c *Controller) Close() error { return c.db.Close() }

func (c *Controller) Status(ctx context.Context, id string) (Run, error) {
	var run Run
	var data []byte
	if err := c.db.QueryRowContext(ctx, "SELECT data FROM runs WHERE id=?", id).Scan(&data); err != nil {
		return run, fmt.Errorf("read run %q: %w", id, err)
	}
	if err := json.Unmarshal(data, &run); err != nil {
		return run, fmt.Errorf("decode run %q: %w", id, err)
	}
	if err := c.loadObservationRevisions(&run); err != nil {
		return run, err
	}
	c.enrichProgress(&run)
	return run, nil
}

func (c *Controller) List(ctx context.Context) ([]Run, error) {
	rows, err := c.db.QueryContext(ctx, "SELECT data FROM runs ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close()
	runs := []Run{}
	for rows.Next() {
		var data []byte
		var run Run
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &run); err != nil {
			return nil, err
		}
		if err := c.loadObservationRevisions(&run); err != nil {
			return nil, err
		}
		c.enrichProgress(&run)
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

// Immediate transactions serialize worker claims, stop requests and completion.
// The callback must not perform remote work or launch a process.
func (c *Controller) update(ctx context.Context, id string, change func(*Run) error) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin run update: %w", err)
	}
	defer tx.Rollback()
	var data []byte
	var run Run
	if err := tx.QueryRowContext(ctx, "SELECT data FROM runs WHERE id=?", id).Scan(&data); err != nil {
		return fmt.Errorf("load run update: %w", err)
	}
	if err := json.Unmarshal(data, &run); err != nil {
		return err
	}
	if err := c.loadObservationRevisions(&run); err != nil {
		return err
	}
	if err := change(&run); err != nil {
		return err
	}
	data, err = json.Marshal(run)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE runs SET data=? WHERE id=?", data, id); err != nil {
		return err
	}
	for _, action := range run.Actions {
		for _, attempt := range action.Attempts {
			if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO attempts VALUES (?,?)", attempt.Token, id); err != nil {
				return err
			}
		}
	}
	if run.State == Succeeded || run.State == Failed || run.State == Stopped {
		if _, err := tx.ExecContext(ctx, "DELETE FROM claims WHERE run_id=?", id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (c *Controller) Submit(ctx context.Context, id string, spec Spec) (Run, error) {
	var run Run
	if !safeName.MatchString(id) {
		return run, errors.New("run ID must be a safe path segment of 1-64 characters")
	}
	if err := spec.Validate(); err != nil {
		return run, fmt.Errorf("validate iteration spec: %w", err)
	}
	// Historical journals retain their original worker and semantic validation.
	// New measurements must satisfy today's admission contract; cleanup sources
	// are historical evidence, not new measurement requests.
	for _, action := range spec.Actions {
		if action.Kind == PerformanceRun {
			if err := action.Performance.ValidateRun(); err != nil {
				return run, fmt.Errorf("admit performance action %q: %w", action.ID, err)
			}
		}
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return run, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "INSERT INTO claims VALUES (?,?)", spec.Environment.ID, id); err != nil {
		return run, fmt.Errorf("claim environment %q: %w", spec.Environment.ID, err)
	}
	dir := filepath.Join(c.root, id)
	if err := os.Mkdir(dir, 0700); err != nil {
		return run, fmt.Errorf("reserve run directory: %w", err)
	}
	// Failed submissions retain their directory as evidence; names are never reused.
	data, err := json.Marshal(spec)
	if err != nil {
		return run, err
	}
	if err := writePrivate(filepath.Join(dir, "spec.json"), data, 0400); err != nil {
		return run, err
	}
	binary, err := os.Open(c.options.BinaryPath)
	if err != nil {
		return run, fmt.Errorf("open worker binary: %w", err)
	}
	defer binary.Close()
	frozen, err := os.OpenFile(filepath.Join(dir, "worker"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0500)
	if err != nil {
		return run, err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(frozen, hash), binary)
	if err := errors.Join(copyErr, frozen.Sync(), frozen.Close()); err != nil {
		return run, fmt.Errorf("freeze worker binary: %w", err)
	}
	run = Run{ID: id, Environment: spec.Environment, State: Prepared, SpecSHA256: fmt.Sprintf("%x", sha256.Sum256(data)), BinarySHA256: fmt.Sprintf("%x", hash.Sum(nil))}
	for _, action := range spec.Actions {
		run.Actions = append(run.Actions, ActionStatus{ID: action.ID, State: Prepared, Attempts: []Attempt{}})
	}
	manifest, err := json.Marshal(run)
	if err != nil {
		return run, err
	}
	if err := writePrivate(filepath.Join(dir, "manifest.json"), manifest, 0400); err != nil {
		return run, err
	}
	if err := syncDirectory(dir); err != nil {
		return run, err
	}
	if err := syncDirectory(c.root); err != nil {
		return run, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO runs VALUES (?,?)", id, manifest); err != nil {
		return run, err
	}
	return run, tx.Commit()
}

func writePrivate(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return fmt.Errorf("create private artifact: %w", err)
	}
	_, writeErr := f.Write(data)
	return errors.Join(writeErr, f.Sync(), f.Close())
}

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

func (c *Controller) loadSpec(run Run) (Spec, error) {
	var spec Spec
	data, err := os.ReadFile(filepath.Join(c.root, run.ID, "spec.json"))
	if err != nil {
		return spec, fmt.Errorf("read frozen spec: %w", err)
	}
	if fmt.Sprintf("%x", sha256.Sum256(data)) != run.SpecSHA256 {
		return spec, errors.New("frozen spec SHA-256 mismatch")
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		return spec, err
	}
	return spec, spec.Validate()
}

func verifyBinary(path, expected string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return err
	}
	if fmt.Sprintf("%x", hash.Sum(nil)) != expected {
		return errors.New("frozen worker SHA-256 mismatch")
	}
	return nil
}
