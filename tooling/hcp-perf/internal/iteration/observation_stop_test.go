package iteration

import "testing"

func TestStoppedObserverReleasesOnlyExitedMeasurement(t *testing.T) {
	c := testController(t, nil)
	spec := startupIterationSpec()
	token := submit(t, c, spec)
	if err := c.update(t.Context(), "run", func(r *Run) error {
		r.StopRequested = true
		r.State = Blocked
		attempt := r.Actions[0].Attempts[0]
		attempt.State = Blocked
		for n := range r.Actions {
			r.Actions[n].State = Stopped
			r.Actions[n].Attempts = nil
		}
		r.Actions[2].State = Blocked
		r.Actions[2].Attempts = []Attempt{attempt}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireLock(c.attemptDir("run", token) + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(t.Context(), "run"); err != nil {
		t.Fatal(err)
	}
	if status(t, c).State != Blocked {
		t.Fatal("cleared locked observer")
	}
	lock.Close()
	if err := c.Reconcile(t.Context(), "run"); err != nil {
		t.Fatal(err)
	}
	if status(t, c).State != Stopped {
		t.Fatal("exited observer not stopped")
	}
}
