#!/usr/bin/env python3
"""Offline Makefile regression tests: python3 hack/test-personal-state.py.

Only the three maintained Makefiles are copied. All producers, tool includes,
and configuration inputs are fixtures; no Go builds or Azure calls are made.
"""

import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import time
import unittest


REPO = Path(__file__).resolve().parents[1]
COMPONENTS = (
    "frontend", "backend", "admin", "sessiongate", "mgmt-agent",
    "kube-applier", "fleet", "aro-hcp-exporter",
)
STUB = r'''#!/usr/bin/env python3
import os
from pathlib import Path
import sys
import time

args = sys.argv[1:]
name = Path(sys.argv[0]).name
if name == "producer":
    component = Path(args[args.index("-C") + 1]).name
    output = Path(next(a.split("=", 1)[1] for a in args if a.startswith("OVERRIDE_CONFIG_FILE=")))
    assert output.parent.stat().st_mode & 0o777 == 0o700
    with open(os.environ["CALLS"], "a") as log:
        log.write(str(output) + "\n")
    time.sleep(float(os.environ.get("DELAY", "0")))
    output.write_text(component + ": complete\n")
    if os.environ.get("FAIL_COMPONENT") == component:
        sys.exit(23)
elif name == "yq":
    if args[0] == "eval-all":
        assert len(args[2:]) == 8, args
        for file in args[2:]:
            print(Path(file).read_text(), end="")
        if os.environ.get("FAIL_MERGE"):
            sys.exit(24)
    elif args[0] == ".serviceGroup":
        print("fixture-service-group")
else:
    flag = "--output" if "--output" in args else "-o"
    output = Path(args[args.index(flag) + 1])
    with open(os.environ["CALLS"], "a") as log:
        log.write(str(output) + "\n")
    output.write_text("GENERATED_USER := " + os.environ["USER"] + "\n")
    if os.environ.get("FAIL_GENERATE"):
        sys.exit(25)
'''


class PersonalStateTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="personal-state-test-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        for name in ("Makefile", "setup-templatize-env.mk", "cluster-service/setup-env.mk"):
            dest = self.root / name
            dest.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(REPO / name, dest)
        for name in (
            ".bingo/Variables.mk", ".bingo/Symlinks.mk", "setup-env.mk",
            "tooling/templatize/Makefile", "tooling/yamlwrap/Makefile", "test/Makefile",
            "config/config.yaml", "tooling/templatize/settings.yaml", "topology.yaml",
            "cluster-service/Env.mk", "cluster-service/pipeline.yaml",
        ):
            dest = self.root / name
            dest.parent.mkdir(parents=True, exist_ok=True)
            dest.write_text("")
        self.bin = self.root / "stub-bin"
        self.bin.mkdir()
        for name in ("producer", "yq", "templatize"):
            dest = self.bin / name
            dest.write_text(STUB)
            dest.chmod(0o700)
        (self.root / "templatize.sh").write_text(STUB)
        (self.root / "templatize.sh").chmod(0o700)
        self.calls = self.root / "calls"
        self.env = dict(os.environ, PATH=f"{self.bin}:{os.environ['PATH']}",
                        CALLS=str(self.calls), USER="fixture-user")
        for key in ("MAKEFLAGS", "MFLAGS", "MAKELEVEL", "ENV_VARS_FILE", "ENV_CACHE_DIR",
                    "PERSONAL_STATE_DIR", "PERS_OVERRIDE_FILE", "CONFIG_FILE", "DEPLOY_ENV",
                    "LOG_LEVEL", "REGION", "RUNS_IN_TEMPLATIZE", "zz_injected_EV2"):
            self.env.pop(key, None)

    def run_make(self, *args, cwd=None, extra_env=None, success=True):
        result = subprocess.run(
            ["make", "--no-print-directory", *args], cwd=cwd or self.root,
            env=self.env | (extra_env or {}), text=True, capture_output=True, timeout=20,
        )
        if success:
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        else:
            self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        return result.stdout

    def override(self, target="latest-services-override", **kwargs):
        return self.run_make(target, "MODULES=", "GOBIN=/nonexistent", "ORAS=/bin/true",
                             f"YQ={self.bin}/yq", f"MAKE={self.bin}/producer", **kwargs)

    def cache(self, legacy=False, args=(), **kwargs):
        include = "setup-env.mk" if legacy else "../setup-templatize-env.mk"
        # Explicit goal prevents the included generation target becoming the default.
        harness = self.root / "cluster-service/harness.mk"
        if not harness.exists():
            harness.write_text(
                "include $(CACHE_SETUP)\n"
                ".PHONY: show\nshow:\n"
                "\t@echo CACHE=$(ENV_VARS_FILE) USER=$(GENERATED_USER)\n"
            )
        output = self.run_make(
            "-f", "harness.mk", "show", f"CACHE_SETUP={include}",
            f"TEMPLATIZE={self.bin}/templatize", *args,
            cwd=self.root / "cluster-service", **kwargs,
        )
        lines = [line for line in output.splitlines() if line.startswith("CACHE=")]
        return Path(lines[-1].split()[0].removeprefix("CACHE=")) if lines else None

    def test_override_producers_and_atomic_failures(self):
        for target in ("record-services-override", "latest-services-override"):
            with self.subTest(target=target):
                self.override(target, extra_env={"DELAY": "0.02"})
                output = self.root / "_artifacts/personal-dev/personal-dev-override.yaml"
                expected = "".join(f"{c}: complete\n" for c in COMPONENTS)
                self.assertEqual(output.read_text(), expected)
                self.assertEqual(output.stat().st_mode & 0o777, 0o600)
                self.assertEqual(output.parent.stat().st_mode & 0o777, 0o700)
                for failure in ({"FAIL_COMPONENT": "backend"}, {"FAIL_MERGE": "1"}):
                    self.override(target, extra_env=failure, success=False)
                    self.assertEqual(output.read_text(), expected)
                self.assertEqual(list(output.parent.iterdir()), [output])
        calls = [Path(p) for p in self.calls.read_text().splitlines()]
        self.assertTrue(all(p.is_relative_to(output.parent) for p in calls))
        self.assertEqual(len({p.parent for p in calls}), 6)

    def test_override_custom_private_paths(self):
        state = self.root / "campaign"
        output = self.root / "private-result/override.yaml"
        self.run_make(
            "latest-services-override", "MODULES=", "GOBIN=/nonexistent",
            f"YQ={self.bin}/yq", f"MAKE={self.bin}/producer",
            f"PERSONAL_STATE_DIR={state}", f"PERS_OVERRIDE_FILE={output}",
        )
        self.assertTrue(output.exists())
        self.assertEqual(state.stat().st_mode & 0o777, 0o700)
        self.override(extra_env={"PERSONAL_STATE_DIR": "relative"}, success=False)
        state.chmod(0o755)
        self.override(extra_env={"PERSONAL_STATE_DIR": str(state)}, success=False)
        state.chmod(0o700)
        link = self.root / "linked-state"
        link.symlink_to(state)
        self.override(extra_env={"PERSONAL_STATE_DIR": str(link)}, success=False)
        output.unlink()
        untouched = state / "user-file"
        untouched.write_text("do not replace")
        output.symlink_to(untouched)
        self.override(extra_env={"PERS_OVERRIDE_FILE": str(output)}, success=False)
        self.assertEqual(untouched.read_text(), "do not replace")

    def test_cache_identity_reuse_and_prerequisites(self):
        for legacy in (False, True):
            with self.subTest(legacy=legacy):
                baseline = self.cache(legacy)
                self.assertEqual(baseline.parent, self.root / "_artifacts/env-cache")
                self.assertEqual(baseline.stat().st_mode & 0o777, 0o600)
                self.assertEqual(baseline.parent.stat().st_mode & 0o777, 0o700)
                before = self.calls.read_text()
                self.assertEqual(self.cache(legacy), baseline)
                self.assertEqual(self.calls.read_text(), before)
                alternate = self.root / "config/alternate.yaml"
                alternate.touch()
                variants = ["USER=another-user", "DEPLOY_ENV=swft", "LOG_LEVEL=7",
                            f"CONFIG_FILE={alternate}"]
                if legacy:
                    variants.extend(("PIPELINE_STEP=other-step", "REGION=other-region",
                                     f"PIPELINE={alternate}"))
                else:
                    variants.extend((f"DEV_SETTINGS_FILE={alternate}",
                                     f"ENV_MK_TMPL={alternate}"))
                keys = {baseline}
                for arg in variants:
                    key = self.cache(legacy, args=(arg,))
                    self.assertNotIn(key, keys, arg)
                    keys.add(key)
                    if arg == "USER=another-user":
                        self.assertIn("another-user", key.read_text())
                for name in ("tooling/templatize/settings.yaml", "config/config.yaml"):
                    time.sleep(0.02)
                    (self.root / name).touch()
                    before = self.calls.read_text()
                    self.assertEqual(self.cache(legacy), baseline)
                    self.assertNotEqual(self.calls.read_text(), before)

    def test_cache_paths_across_working_directories_and_overrides(self):
        baseline = self.cache()
        before = self.calls.read_text()
        output = self.run_make(
            "-f", "cluster-service/harness.mk", "show",
            f"CACHE_SETUP={self.root}/setup-templatize-env.mk",
            f"ENV_MK_TMPL={self.root}/cluster-service/Env.mk",
            f"TEMPLATIZE={self.bin}/templatize",
        )
        self.assertIn(f"CACHE={baseline}", output)
        self.assertEqual(self.calls.read_text(), before)
        for legacy in (False, True):
            custom = self.root / "run-cache"
            cache = self.cache(legacy, args=(f"ENV_CACHE_DIR={custom}",))
            self.assertEqual(cache.parent, custom)
            explicit = self.root / "explicit-cache/vars.mk"
            cache = self.cache(legacy, args=(f"ENV_VARS_FILE={explicit}",))
            self.assertEqual(cache, explicit)

    def test_cache_failure_preserves_previous_output(self):
        for legacy in (False, True):
            with self.subTest(legacy=legacy):
                baseline = self.cache(legacy)
                previous = baseline.read_bytes()
                time.sleep(0.02)
                (self.root / "config/config.yaml").touch()
                self.cache(legacy, extra_env={"FAIL_GENERATE": "1"}, success=False)
                self.assertEqual(baseline.read_bytes(), previous)
                self.assertFalse(list(baseline.parent.glob(baseline.name + ".*")))
                self.cache(legacy, args=("ENV_CACHE_DIR=relative",), success=False)
                public = self.root / "public-cache"
                public.mkdir(exist_ok=True)
                public.chmod(0o755)
                self.cache(legacy, args=(f"ENV_CACHE_DIR={public}",), success=False)
                missing = self.root / f"missing-{legacy}/vars.mk"
                self.cache(legacy, args=(f"ENV_VARS_FILE={missing}",),
                           extra_env={"FAIL_GENERATE": "1"}, success=False)
                self.assertFalse(missing.exists())


if __name__ == "__main__":
    unittest.main()
