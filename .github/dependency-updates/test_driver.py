"""Exercise dependency promotion against real, isolated Git repositories.

Only the provider's authoritative check lookup is replaced by a callback. Git
inspection, fetching, branch movement, and push rejection use local bare remotes.
"""

from __future__ import annotations

import copy
import base64
import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import unittest
from unittest import mock


DRIVER_PATH = Path(__file__).with_name("driver.py")
SPEC = importlib.util.spec_from_file_location("dependency_update_driver", DRIVER_PATH)
assert SPEC is not None and SPEC.loader is not None
driver = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = driver
SPEC.loader.exec_module(driver)


class GitFixture(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory(prefix="dependency-promotion-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.repo = self.root / "checkout"
        self.remote = self.root / "remote.git"
        self.repo.mkdir()
        self._git(self.root, "init", "--bare", "--initial-branch=develop", str(self.remote))
        self._git(self.repo, "init", "--initial-branch=develop")
        self._git(self.repo, "config", "user.name", "Dependency Fixture")
        self._git(self.repo, "config", "user.email", "fixture@example.invalid")
        self._git(self.repo, "config", "core.filemode", "true")
        (self.repo / "go.mod").write_text("module fixture/repo\n\ngo 1.26.0\n")
        (self.repo / "go.sum").write_text("fixture-dependency v1.0.0 h1:fixture\n")
        (self.repo / "app.py").write_text("print('application')\n")
        self._commit("Baseline")
        self.baseline = self._git(self.repo, "rev-parse", "HEAD")
        self._git(self.repo, "remote", "add", "origin", str(self.remote))
        self._git(self.repo, "push", "origin", "develop")
        self.branch = "dependency-updates/fixture"
        self.policy = {
            "repository": "fixture/repo",
            "defaultBranch": "develop",
            "candidatePrefix": "dependency-updates/",
            "allowedPaths": ["go.mod", "go.sum", "vendor/**"],
        }

    @staticmethod
    def _git(directory: Path, *arguments: str) -> str:
        result = subprocess.run(
            ["git", "-C", str(directory), *arguments],
            check=True,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env={
                **os.environ,
                "GIT_CONFIG_NOSYSTEM": "1",
                "GIT_CONFIG_GLOBAL": os.devnull,
                "GIT_TERMINAL_PROMPT": "0",
            },
        )
        return result.stdout.strip()

    def _commit(self, message: str) -> str:
        self._git(self.repo, "add", "--all")
        self._git(self.repo, "commit", "--quiet", "-m", message)
        return self._git(self.repo, "rev-parse", "HEAD")

    def _candidate(self, changes: dict[str, str] | None = None) -> str:
        self._git(self.repo, "checkout", "--quiet", "-b", self.branch, self.baseline)
        for name, content in (changes or {"go.sum": "fixture-dependency v1.0.1 h1:new\n"}).items():
            path = self.repo / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(content)
        candidate = self._commit("Update compatible dependency")
        self._git(self.repo, "push", "origin", self.branch)
        return candidate

    def _receipt(self, candidate: str) -> dict:
        return driver.inspect_candidate(
            self.repo, self.baseline, candidate, self.policy, self.branch
        )

    def _remote_head(self, branch: str = "develop") -> str:
        return self._git(self.remote, "rev-parse", f"refs/heads/{branch}")

    def _competing_commit(self) -> str:
        tree = self._git(self.repo, "rev-parse", f"{self.baseline}^{{tree}}")
        competitor = self._git(
            self.repo, "commit-tree", tree, "-p", self.baseline,
            "-m", "Independent default-branch change",
        )
        self._git(self.repo, "push", "origin", f"{competitor}:refs/heads/competing")
        return competitor

    def _reject_promotion(self, receipt: dict, verify_checks=None) -> None:
        before = self._remote_head()
        verifier = verify_checks or mock.Mock(return_value=None)
        with self.assertRaises(Exception):
            driver.promote_candidate(self.repo, receipt, self.policy, verifier)
        self.assertEqual(self._remote_head(), before)


class PromotionFixture(GitFixture):
    def test_allowed_dependency_candidate_is_inspected_and_promoted(self) -> None:
        candidate = self._candidate()
        receipt = self._receipt(candidate)
        self.assertEqual(receipt["schemaVersion"], 1)
        self.assertEqual(receipt["repository"], "fixture/repo")
        self.assertEqual(receipt["defaultBranch"], "develop")
        self.assertEqual(receipt["baseline"], self.baseline)
        self.assertEqual(receipt["candidate"], candidate)
        self.assertEqual(receipt["branch"], self.branch)
        self.assertEqual(receipt["changedFiles"], ["go.sum"])
        self.assertTrue(receipt["policyDigest"])
        verified = mock.Mock(return_value=None)
        promoted = driver.promote_candidate(self.repo, receipt, self.policy, verified)
        verified.assert_called_once_with(receipt)
        self.assertEqual(promoted, candidate)
        self.assertEqual(self._remote_head(), candidate)

    def test_failed_missing_and_forged_check_evidence_cannot_promote(self) -> None:
        candidate = self._candidate()
        receipt = self._receipt(candidate)
        for reason in ("checks failed", "checks missing", "forged evidence"):
            with self.subTest(reason=reason):
                verified = mock.Mock(side_effect=RuntimeError(reason))
                self._reject_promotion(receipt, verified)
                verified.assert_called_once_with(receipt)

    def test_self_reported_success_cannot_replace_authoritative_checks(self) -> None:
        receipt = self._receipt(self._candidate())
        receipt["checksPassed"] = True
        verified = mock.Mock(side_effect=RuntimeError("No authoritative successful run"))
        self._reject_promotion(receipt, verified)
        verified.assert_called_once_with(receipt)

    def test_receipt_tampering_cannot_promote(self) -> None:
        receipt = self._receipt(self._candidate())
        tampering = {
            "changedFiles": [],
            "policyDigest": "0" * 64,
            "repository": "other/repository",
            "defaultBranch": "main",
            "schemaVersion": 99,
            "extraTrustedProof": "fabricated",
        }
        for field, value in tampering.items():
            with self.subTest(field=field):
                tampered = copy.deepcopy(receipt)
                tampered[field] = value
                verified = mock.Mock(return_value=None)
                self._reject_promotion(tampered, verified)
                verified.assert_called_once_with(tampered)

    def test_changed_trusted_policy_invalidates_receipt(self) -> None:
        receipt = self._receipt(self._candidate())
        self.policy["allowedPaths"].append("another.lock")
        self._reject_promotion(receipt)

    def test_stale_default_branch_cannot_promote(self) -> None:
        receipt = self._receipt(self._candidate())
        competitor = self._competing_commit()
        self._git(self.remote, "update-ref", "refs/heads/develop", competitor, self.baseline)
        self._reject_promotion(receipt)
        self.assertEqual(self._remote_head(), competitor)

    def test_remote_candidate_change_cannot_promote_old_receipt(self) -> None:
        candidate = self._candidate()
        receipt = self._receipt(candidate)
        (self.repo / "go.sum").write_text("fixture-dependency v1.0.2 h1:newer\n")
        newer = self._commit("Candidate changed after checks")
        self._git(self.repo, "push", "origin", self.branch)
        self._reject_promotion(receipt)
        self.assertEqual(self._remote_head(self.branch), newer)

    def test_deleted_remote_candidate_cannot_promote(self) -> None:
        receipt = self._receipt(self._candidate())
        self._git(self.remote, "update-ref", "-d", f"refs/heads/{self.branch}")
        self._reject_promotion(receipt)

    def test_default_advancing_between_fetch_and_push_is_not_overwritten(self) -> None:
        candidate = self._candidate()
        receipt = self._receipt(candidate)
        competitor = self._competing_commit()
        original_run = subprocess.run
        intercepted = []

        def race_at_push(command, *arguments, **kwargs):
            if isinstance(command, (list, tuple)) and command and Path(str(command[0])).name == "git" and "push" in command:
                intercepted.append(list(command))
                self.assertFalse(any(
                    str(option).startswith("--force") or option == "-f" or str(option).startswith("+")
                    for option in command
                ), f"Promotion must not force push: {command!r}")
                original_run(
                    ["git", "-C", str(self.remote), "update-ref", "refs/heads/develop", competitor, self.baseline],
                    check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                )
            return original_run(command, *arguments, **kwargs)

        with mock.patch.object(subprocess, "run", side_effect=race_at_push):
            with self.assertRaises(Exception):
                driver.promote_candidate(self.repo, receipt, self.policy, mock.Mock(return_value=None))
        self.assertEqual(len(intercepted), 1, "Fixture must reach the actual promotion push")
        self.assertEqual(self._remote_head(), competitor)
        self.assertEqual(self._remote_head(self.branch), candidate)

    def test_application_file_is_rejected(self) -> None:
        candidate = self._candidate({"app.py": "print('changed application')\n"})
        with self.assertRaises(Exception):
            self._receipt(candidate)
        self.assertEqual(self._remote_head(), self.baseline)

    def test_workflow_file_is_rejected(self) -> None:
        candidate = self._candidate({".github/workflows/ci.yml": "name: candidate-controlled\n"})
        with self.assertRaises(Exception):
            self._receipt(candidate)
        self.assertEqual(self._remote_head(), self.baseline)

    def test_new_regular_vendored_dependency_file_is_allowed(self) -> None:
        candidate = self._candidate({"vendor/example.invalid/pkg/dependency.go": "package dependency\n"})
        receipt = self._receipt(candidate)
        self.assertEqual(receipt["changedFiles"], ["vendor/example.invalid/pkg/dependency.go"])

    def test_removed_regular_dependency_file_is_allowed(self) -> None:
        self._git(self.repo, "checkout", "--quiet", "-b", self.branch, self.baseline)
        (self.repo / "go.sum").unlink()
        candidate = self._commit("Remove obsolete dependency checksum file")
        receipt = self._receipt(candidate)
        self.assertEqual(receipt["changedFiles"], ["go.sum"])

    def test_empty_candidate_is_rejected(self) -> None:
        self._git(self.repo, "checkout", "--quiet", "-b", self.branch, self.baseline)
        self._git(self.repo, "commit", "--quiet", "--allow-empty", "-m", "No dependency update")
        candidate = self._git(self.repo, "rev-parse", "HEAD")
        with self.assertRaises(Exception):
            self._receipt(candidate)

    def test_symlink_in_allowed_dependency_path_is_rejected(self) -> None:
        self._git(self.repo, "checkout", "--quiet", "-b", self.branch, self.baseline)
        (self.repo / "go.sum").unlink()
        (self.repo / "go.sum").symlink_to("app.py")
        candidate = self._commit("Replace dependency manifest with symlink")
        with self.assertRaises(Exception):
            self._receipt(candidate)

    def test_new_symlink_under_vendor_is_rejected(self) -> None:
        self._git(self.repo, "checkout", "--quiet", "-b", self.branch, self.baseline)
        (self.repo / "vendor").mkdir()
        (self.repo / "vendor" / "dependency.go").symlink_to("../app.py")
        candidate = self._commit("Add symlink in vendored dependency")
        with self.assertRaises(Exception):
            self._receipt(candidate)

    def test_dependency_manifest_executable_mode_change_is_rejected(self) -> None:
        self._git(self.repo, "checkout", "--quiet", "-b", self.branch, self.baseline)
        os.chmod(self.repo / "go.mod", 0o755)
        candidate = self._commit("Make dependency manifest executable")
        with self.assertRaises(Exception):
            self._receipt(candidate)

    def test_new_executable_under_vendor_is_rejected(self) -> None:
        self._git(self.repo, "checkout", "--quiet", "-b", self.branch, self.baseline)
        path = self.repo / "vendor" / "dependency.sh"
        path.parent.mkdir()
        path.write_text("#!/bin/sh\nexit 0\n")
        path.chmod(0o755)
        candidate = self._commit("Add executable vendored file")
        with self.assertRaises(Exception):
            self._receipt(candidate)

    def test_multiple_candidate_commits_are_rejected(self) -> None:
        self._candidate()
        (self.repo / "go.sum").write_text("fixture-dependency v1.0.2 h1:newer\n")
        candidate = self._commit("Second unchecked candidate commit")
        with self.assertRaises(Exception):
            self._receipt(candidate)

    def test_merge_candidate_is_rejected(self) -> None:
        valid = self._candidate()
        other = self._competing_commit()
        tree = self._git(self.repo, "rev-parse", f"{valid}^{{tree}}")
        candidate = self._git(
            self.repo, "commit-tree", tree, "-p", self.baseline, "-p", other,
            "-m", "Merge candidate with two parents",
        )
        with self.assertRaises(Exception):
            self._receipt(candidate)

    def test_candidate_from_another_baseline_is_rejected(self) -> None:
        candidate = self._candidate()
        other = self._competing_commit()
        with self.assertRaises(Exception):
            driver.inspect_candidate(self.repo, other, candidate, self.policy, self.branch)

    def test_candidate_branch_outside_reserved_prefix_is_rejected(self) -> None:
        candidate = self._candidate()
        with self.assertRaises(Exception):
            driver.inspect_candidate(self.repo, self.baseline, candidate, self.policy, "feature/manual")


class CandidateCleanupFixture(GitFixture):
    def setUp(self) -> None:
        super().setUp()
        self.branch = "dependency-updates/42-3/fixture"
        self.environment = {"GITHUB_RUN_ID": "42", "GITHUB_RUN_ATTEMPT": "3"}

    def _promoted_receipt(self) -> dict:
        receipt = self._receipt(self._candidate())
        driver.promote_candidate(self.repo, receipt, self.policy, mock.Mock(return_value=None))
        return receipt

    def test_unchanged_promoted_candidate_is_removed_without_changing_default(self) -> None:
        receipt = self._promoted_receipt()
        with mock.patch.dict(os.environ, self.environment):
            driver.cleanup_promoted_candidate(self.repo, receipt, self.policy)
        self.assertEqual(self._git(self.repo, "ls-remote", "--heads", "origin", "refs/heads/" + self.branch), "")
        self.assertEqual(self._remote_head(), receipt["candidate"])

    def test_changed_candidate_ref_is_retained_after_successful_promotion(self) -> None:
        receipt = self._promoted_receipt()
        (self.repo / "go.sum").write_text("user's newer dependency change\n")
        changed = self._commit("User continued candidate work")
        self._git(self.repo, "push", "origin", self.branch)
        with mock.patch.dict(os.environ, self.environment):
            with self.assertRaises(subprocess.CalledProcessError):
                driver.cleanup_promoted_candidate(self.repo, receipt, self.policy)
        self.assertEqual(self._remote_head(self.branch), changed)
        self.assertEqual(self._remote_head(), receipt["candidate"])

    def test_ref_advancing_during_cleanup_push_is_retained_by_exact_sha_lease(self) -> None:
        receipt = self._promoted_receipt()
        tree = self._git(self.repo, "rev-parse", receipt["candidate"] + "^{tree}")
        changed = self._git(self.repo, "commit-tree", tree, "-p", receipt["candidate"], "-m", "Concurrent user change")
        self._git(self.repo, "push", "origin", changed + ":refs/heads/cleanup-race-object")
        original_run = subprocess.run
        intercepted = []

        def race_at_cleanup(command, *arguments, **kwargs):
            if command[0] == "git" and "push" in command:
                intercepted.append(command)
                ref = "refs/heads/" + self.branch
                self.assertIn("--force-with-lease=" + ref + ":" + receipt["candidate"], command)
                self.assertIn(":" + ref, command)
                self.assertNotIn(":refs/heads/develop", command)
                original_run(
                    ["git", "-C", str(self.remote), "update-ref", ref, changed, receipt["candidate"]],
                    check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                )
            return original_run(command, *arguments, **kwargs)

        with mock.patch.dict(os.environ, self.environment):
            with mock.patch.object(subprocess, "run", side_effect=race_at_cleanup):
                with self.assertRaises(subprocess.CalledProcessError):
                    driver.cleanup_promoted_candidate(self.repo, receipt, self.policy)
        self.assertEqual(len(intercepted), 1)
        self.assertEqual(self._remote_head(self.branch), changed)
        self.assertEqual(self._remote_head(), receipt["candidate"])

    def test_cleanup_cannot_touch_another_run_attempt_or_default_branch(self) -> None:
        receipt = self._promoted_receipt()
        cases = [
            ({**self.environment, "GITHUB_RUN_ID": "43"}, receipt),
            ({**self.environment, "GITHUB_RUN_ATTEMPT": "4"}, receipt),
            (self.environment, {**receipt, "branch": "develop"}),
        ]
        for environment, candidate_receipt in cases:
            with self.subTest(environment=environment, branch=candidate_receipt["branch"]):
                with mock.patch.dict(os.environ, environment):
                    with mock.patch.object(subprocess, "run") as mutation:
                        with self.assertRaises(ValueError):
                            driver.cleanup_promoted_candidate(self.repo, candidate_receipt, self.policy)
                        mutation.assert_not_called()
        self.assertEqual(self._remote_head(self.branch), receipt["candidate"])
        self.assertEqual(self._remote_head(), receipt["candidate"])

    def test_separate_cleanup_failure_is_diagnostic_after_exact_promotion_summary(self) -> None:
        candidate = self._candidate()
        policy_path = self.root / "policy.json"
        policy_path.write_text(json.dumps(self.policy))
        summary = self.root / "summary.md"
        receipt_path = self.root / "promoted.json"
        arguments = [
            "driver.py", "promote", "--policy", str(policy_path), "--baseline", self.baseline,
            "--candidate", candidate, "--branch", self.branch, "--receipt", str(receipt_path),
        ]
        original_cwd = Path.cwd()
        diagnostic = io.StringIO()
        try:
            os.chdir(self.repo)
            with mock.patch.dict(os.environ, {**self.environment, "GITHUB_STEP_SUMMARY": str(summary)}):
                with mock.patch.object(driver, "verify_workflow_checks", return_value=None) as authoritative_checks:
                    with mock.patch.object(driver, "cleanup_promoted_candidate", side_effect=subprocess.CalledProcessError(1, ["git", "push"])) as cleanup:
                        with mock.patch.object(sys, "argv", arguments):
                            driver.main()
                        cleanup.assert_not_called()
                        persisted = json.loads(receipt_path.read_text())
                        persisted_summary = summary.read_text()
                        self.assertIn("\n```json\n", persisted_summary)
                        summary_receipt = json.loads(persisted_summary.split("\n```json\n", 1)[1].split("\n```", 1)[0])
                        self.assertEqual(summary_receipt, persisted)
                        self.assertEqual(self._remote_head(), candidate)
                        with mock.patch.object(sys, "argv", ["driver.py", "cleanup", "--policy", str(policy_path), "--receipt", str(receipt_path)]):
                            with contextlib.redirect_stdout(diagnostic):
                                driver.main()
                        cleanup.assert_called_once_with(self.repo, persisted, self.policy)
                        self.assertEqual(authoritative_checks.call_count, 2)
        finally:
            os.chdir(original_cwd)
        self.assertEqual(self._remote_head(), candidate)
        self.assertEqual(self._remote_head(self.branch), candidate)
        self.assertEqual(json.loads(receipt_path.read_text())["candidate"], candidate)
        self.assertIn("source promotion remains successful", diagnostic.getvalue())
        self.assertIn("source promotion remains successful", summary.read_text())

    def test_cleanup_cli_holds_an_unpromoted_candidate_before_ref_deletion(self) -> None:
        receipt = self._receipt(self._candidate())
        policy_path, receipt_path = self.root / "policy.json", self.root / "receipt.json"
        policy_path.write_text(json.dumps(self.policy))
        receipt_path.write_text(json.dumps(receipt))
        original_cwd = Path.cwd()
        try:
            os.chdir(self.repo)
            with mock.patch.dict(os.environ, {**self.environment, "GITHUB_STEP_SUMMARY": str(self.root / "summary.md")}):
                with mock.patch.object(sys, "argv", ["driver.py", "cleanup", "--policy", str(policy_path), "--receipt", str(receipt_path)]):
                    with mock.patch.object(driver, "verify_workflow_checks", return_value=None):
                        with mock.patch.object(driver, "cleanup_promoted_candidate") as deletion:
                            with contextlib.redirect_stdout(io.StringIO()):
                                driver.main()
                            deletion.assert_not_called()
        finally:
            os.chdir(original_cwd)
        self.assertEqual(self._remote_head(self.branch), receipt["candidate"])
        self.assertEqual(self._remote_head(), self.baseline)

    def test_cleanup_cli_rejects_tampered_receipt_before_authorization_or_deletion(self) -> None:
        receipt = self._promoted_receipt()
        receipt["changedFiles"] = []
        policy_path, receipt_path = self.root / "policy.json", self.root / "receipt.json"
        policy_path.write_text(json.dumps(self.policy))
        receipt_path.write_text(json.dumps(receipt))
        original_cwd = Path.cwd()
        try:
            os.chdir(self.repo)
            with mock.patch.dict(os.environ, {**self.environment, "GITHUB_STEP_SUMMARY": str(self.root / "summary.md")}):
                with mock.patch.object(sys, "argv", ["driver.py", "cleanup", "--policy", str(policy_path), "--receipt", str(receipt_path)]):
                    with mock.patch.object(driver, "verify_workflow_checks") as authorization:
                        with mock.patch.object(driver, "cleanup_promoted_candidate") as deletion:
                            with contextlib.redirect_stdout(io.StringIO()):
                                driver.main()
                            authorization.assert_not_called()
                            deletion.assert_not_called()
        finally:
            os.chdir(original_cwd)
        self.assertEqual(self._remote_head(self.branch), receipt["candidate"])
        self.assertEqual(self._remote_head(), receipt["candidate"])


class WorkflowCheckFixture(unittest.TestCase):
    def setUp(self) -> None:
        self.receipt = {
            "repository": "fixture/repo",
            "baseline": "1" * 40,
            "candidate": "2" * 40,
        }
        self.run = {
            "id": 42,
            "repository": {"full_name": "fixture/repo"},
            "head_sha": self.receipt["baseline"],
            "run_attempt": 3,
            "path": driver.WORKFLOW,
            "event": "schedule",
        }
        self.success = {
            "name": driver.CHECK_JOB,
            "status": "completed",
            "conclusion": "success",
        }
        self.environment = {
            "GITHUB_REPOSITORY": "fixture/repo",
            "GITHUB_RUN_ID": "42",
            "GITHUB_RUN_ATTEMPT": "3",
            "GITHUB_TOKEN": "fixture-token",
        }

    def _verify(self, run=None, jobs=None, environment=None):
        responses = [
            copy.deepcopy(self.run if run is None else run),
            {"jobs": copy.deepcopy([self.success] if jobs is None else jobs)},
        ]
        with mock.patch.dict(os.environ, environment or self.environment):
            with mock.patch.object(driver, "api", side_effect=responses) as authoritative_api:
                driver.verify_workflow_checks(self.receipt)
        return authoritative_api

    def test_exact_authoritative_run_and_attempt_succeed(self) -> None:
        authoritative_api = self._verify()
        self.assertEqual(authoritative_api.call_args_list, [
            mock.call("/repos/fixture/repo/actions/runs/42", "fixture-token"),
            mock.call("/repos/fixture/repo/actions/runs/42/attempts/3/jobs?per_page=100", "fixture-token"),
        ])

    def test_manual_dispatch_at_exact_trusted_baseline_succeeds(self) -> None:
        run = {**self.run, "event": "workflow_dispatch"}
        self._verify(run=run)

    def test_workflow_path_with_authoritative_ref_suffix_succeeds(self) -> None:
        self._verify(run={**self.run, "path": driver.WORKFLOW + "@refs/heads/develop"})

    def test_unrelated_jobs_do_not_replace_or_confuse_exact_validation_job(self) -> None:
        unrelated = {"name": "Generate candidate", "status": "completed", "conclusion": "success"}
        self._verify(jobs=[unrelated, self.success])

    def test_wrong_workflow_run_metadata_are_rejected(self) -> None:
        cases = {
            "head_sha": self.receipt["candidate"],
            "run_attempt": 4,
            "path": ".github/workflows/untrusted.yml",
            "event": "pull_request",
        }
        for field, value in cases.items():
            with self.subTest(field=field):
                with self.assertRaises(ValueError):
                    self._verify(run={**self.run, field: value})

    def test_push_event_and_near_match_workflow_path_are_rejected(self) -> None:
        for field, value in (("event", "push"), ("path", driver.WORKFLOW + ".untrusted")):
            with self.subTest(field=field):
                with self.assertRaises(ValueError):
                    self._verify(run={**self.run, field: value})

    def test_receipt_for_another_repository_is_rejected_before_api_lookup(self) -> None:
        self.receipt["repository"] = "unrelated/repository"
        with mock.patch.dict(os.environ, self.environment):
            with mock.patch.object(driver, "api") as authoritative_api:
                with self.assertRaises(ValueError):
                    driver.verify_workflow_checks(self.receipt)
                authoritative_api.assert_not_called()

    def test_current_job_repository_must_match_receipt(self) -> None:
        environment = {**self.environment, "GITHUB_REPOSITORY": "unrelated/repository"}
        with self.assertRaises(ValueError):
            self._verify(environment=environment)

    def test_pending_validation_jobs_are_rejected(self) -> None:
        for status, conclusion in (("queued", None), ("in_progress", None), ("in_progress", "success")):
            with self.subTest(status=status, conclusion=conclusion):
                job = {**self.success, "status": status, "conclusion": conclusion}
                with self.assertRaises(ValueError):
                    self._verify(jobs=[job])

    def test_unsuccessful_completed_validation_jobs_are_rejected(self) -> None:
        for conclusion in ("failure", "cancelled", "skipped", "neutral", "timed_out", None):
            with self.subTest(conclusion=conclusion):
                with self.assertRaises(ValueError):
                    self._verify(jobs=[{**self.success, "conclusion": conclusion}])

    def test_multiple_exact_validation_jobs_are_rejected(self) -> None:
        with self.assertRaises(ValueError):
            self._verify(jobs=[self.success, self.success])

    def test_missing_exact_validation_job_is_rejected(self) -> None:
        for jobs in ([], [{**self.success, "name": driver.CHECK_JOB + " (other)"}]):
            with self.subTest(jobs=jobs):
                with self.assertRaises(ValueError):
                    self._verify(jobs=jobs)


class VendorReconstructionFixture(GitFixture):
    def setUp(self) -> None:
        super().setUp()
        self.policy["canonicalVendorRequired"] = True
        self.go_environment = {
            "GOPROXY": "off",
            "GOSUMDB": "off",
            "GOTOOLCHAIN": "local",
            "GOWORK": "off",
            "GOCACHE": str(self.root / "go-cache"),
            "GOMODCACHE": str(self.root / "go-mod-cache"),
        }
        (self.repo / "go.mod").write_text(
            "module fixture/repo\n\ngo 1.26.0\n\n"
            "require example.invalid/dependency v1.0.0\n\n"
            "replace example.invalid/dependency => ./fixtures/dependency\n"
        )
        (self.repo / "go.sum").write_text("")
        (self.repo / "main.go").write_text(
            'package main\n\nimport "example.invalid/dependency"\n\n'
            "func main() { println(dependency.Value) }\n"
        )
        dependency = self.repo / "fixtures" / "dependency"
        dependency.mkdir(parents=True)
        (dependency / "go.mod").write_text("module example.invalid/dependency\n\ngo 1.26.0\n")
        (dependency / "value.go").write_text('package dependency\n\nconst Value = "original"\n')
        with mock.patch.dict(os.environ, self.go_environment):
            subprocess.run(
                ["go", "mod", "vendor"], cwd=self.repo, check=True,
                stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            )
        self.baseline = self._commit("Canonical local Go dependency vendor")
        self._git(self.repo, "push", "origin", "develop")

    def _snapshot(self) -> tuple[str, str, str, str]:
        return (
            self._git(self.repo, "rev-parse", "HEAD"),
            self._git(self.repo, "status", "--porcelain"),
            self._git(self.repo, "diff", "HEAD"),
            self._git(self.repo, "worktree", "list", "--porcelain"),
        )

    def _make_original_worktree_dirty(self) -> None:
        (self.repo / "app.py").write_text("print('uncommitted application work')\n")
        (self.repo / "local-untracked.txt").write_text("preserve this untracked work\n")

    def test_real_canonical_vendor_reconstruction_preserves_original_worktree(self) -> None:
        self._make_original_worktree_dirty()
        before = self._snapshot()
        with mock.patch.dict(os.environ, self.go_environment):
            self.assertTrue(driver.canonical_vendor(self.repo, self.baseline))
        self.assertEqual(self._snapshot(), before)
        self.assertEqual((self.repo / "local-untracked.txt").read_text(), "preserve this untracked work\n")

    def test_reviewed_vendor_patch_is_detected_without_overwriting_original_worktree(self) -> None:
        patched = self.repo / "vendor" / "example.invalid" / "dependency" / "value.go"
        patched.write_text('package dependency\n\nconst Value = "reviewed platform patch"\n')
        self.baseline = self._commit("Reviewed local vendored patch")
        self._make_original_worktree_dirty()
        before = self._snapshot()
        with mock.patch.dict(os.environ, self.go_environment):
            self.assertFalse(driver.canonical_vendor(self.repo, self.baseline))
        self.assertEqual(self._snapshot(), before)
        self.assertIn("reviewed platform patch", patched.read_text())

    def test_canonical_baseline_does_not_authorize_tampered_candidate_vendor_bytes(self) -> None:
        with mock.patch.dict(os.environ, self.go_environment):
            self.assertTrue(driver.canonical_vendor(self.repo, self.baseline))
        candidate = self._candidate({
            "vendor/example.invalid/dependency/value.go": 'package dependency\n\nconst Value = "tampered candidate"\n',
        })
        before = self._snapshot()
        with mock.patch.dict(os.environ, self.go_environment):
            with self.assertRaisesRegex(ValueError, "candidate vendor bytes"):
                driver.verify_candidate_vendor(self.repo, self.baseline, candidate, self.policy, self.branch, "true")
        self.assertEqual(self._snapshot(), before)
        self.assertEqual(self._remote_head(), self.baseline)

    def test_regenerated_candidate_vendor_matches_its_exact_updated_module_inputs(self) -> None:
        self._git(self.repo, "checkout", "--quiet", "-b", self.branch, self.baseline)
        module = self.repo / "go.mod"
        module.write_text(module.read_text().replace("dependency v1.0.0", "dependency v1.0.1"))
        with mock.patch.dict(os.environ, self.go_environment):
            subprocess.run(
                ["go", "mod", "vendor"], cwd=self.repo, check=True,
                stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            )
        candidate = self._commit("Regenerate vendor for compatible dependency update")
        self._git(self.repo, "push", "origin", self.branch)
        before = self._snapshot()
        self.assertIn("dependency v1.0.1", (self.repo / "vendor/modules.txt").read_text())
        with mock.patch.dict(os.environ, self.go_environment):
            driver.verify_candidate_vendor(self.repo, self.baseline, candidate, self.policy, self.branch, "true")
        self.assertEqual(self._snapshot(), before)

    def test_unrelated_npm_candidate_preserves_noncanonical_vendor_patches(self) -> None:
        patched = self.repo / "vendor/example.invalid/dependency/value.go"
        patched.write_text('package dependency\n\nconst Value = "reviewed platform patch"\n')
        self.baseline = self._commit("Reviewed local vendored patch")
        with mock.patch.dict(os.environ, self.go_environment):
            self.assertFalse(driver.canonical_vendor(self.repo, self.baseline))
        self.policy["allowedPaths"].append("package-lock.json")
        candidate = self._candidate({"package-lock.json": '{"lockfileVersion":3,"packages":{}}\n'})
        before = self._snapshot()
        with mock.patch.object(driver, "canonical_vendor", side_effect=AssertionError("Npm updates must preserve vendor patches")) as reconstruction:
            driver.verify_candidate_vendor(self.repo, self.baseline, candidate, self.policy, self.branch, "false")
            reconstruction.assert_not_called()
        self.assertEqual(self._snapshot(), before)
        self.assertIn("reviewed platform patch", patched.read_text())


class CandidateCleanlinessFixture(GitFixture):
    def test_exact_clean_tested_candidate_succeeds(self) -> None:
        candidate = self._candidate()
        driver.require_clean_candidate(self.repo, candidate)

    def test_vendor_verification_requires_the_exact_candidate_head(self) -> None:
        candidate = self._candidate()
        self._git(self.repo, "checkout", "--quiet", "--detach", self.baseline)
        with self.assertRaisesRegex(ValueError, "checkout changed"):
            driver.verify_candidate_vendor(self.repo, self.baseline, candidate, self.policy, self.branch, "true")

    def test_successful_build_that_changes_checksum_file_is_rejected(self) -> None:
        candidate = self._candidate()
        completed = subprocess.run(
            [sys.executable, "-c", "from pathlib import Path; Path('go.sum').write_text('build-induced checksum drift\\n')"],
            cwd=self.repo, check=True,
        )
        self.assertEqual(completed.returncode, 0)
        with self.assertRaises(ValueError):
            driver.require_clean_candidate(self.repo, candidate)
        self.assertEqual(self._remote_head(), self.baseline)

    def test_successful_build_that_stages_dependency_drift_is_rejected(self) -> None:
        candidate = self._candidate()
        (self.repo / "go.mod").write_text("module fixture/repo\n\ngo 1.26.1\n")
        self._git(self.repo, "add", "go.mod")
        with self.assertRaises(ValueError):
            driver.require_clean_candidate(self.repo, candidate)

    def test_successful_build_that_changes_node_lockfile_is_rejected(self) -> None:
        lockfile = self.repo / "package-lock.json"
        lockfile.write_text('{"name":"fixture","lockfileVersion":3}\n')
        self.baseline = self._commit("Baseline Node dependency lockfile")
        self._git(self.repo, "push", "origin", "develop")
        candidate = self._candidate()
        completed = subprocess.run(
            [sys.executable, "-c", "from pathlib import Path; Path('package-lock.json').write_text('{\"lockfileVersion\":3,\"packages\":{\"new\":{}}}\\n')"],
            cwd=self.repo, check=True,
        )
        self.assertEqual(completed.returncode, 0)
        with self.assertRaises(ValueError):
            driver.require_clean_candidate(self.repo, candidate)

    def test_new_untracked_node_lockfile_is_rejected(self) -> None:
        candidate = self._candidate()
        (self.repo / "package-lock.json").write_text('{"lockfileVersion":3,"packages":{}}\n')
        with self.assertRaises(ValueError):
            driver.require_clean_candidate(self.repo, candidate)

    def test_recreated_untracked_checksum_file_is_rejected(self) -> None:
        self._git(self.repo, "checkout", "--quiet", "-b", self.branch, self.baseline)
        (self.repo / "go.sum").unlink()
        candidate = self._commit("Remove obsolete checksum file")
        self._git(self.repo, "push", "origin", self.branch)
        (self.repo / "go.sum").write_text("build-created dependency checksum\n")
        with self.assertRaises(ValueError):
            driver.require_clean_candidate(self.repo, candidate)

    def test_ignored_build_output_does_not_change_candidate_inputs(self) -> None:
        (self.repo / ".gitignore").write_text("*.test-output\n")
        self.baseline = self._commit("Baseline ignores for generated build outputs")
        self._git(self.repo, "push", "origin", "develop")
        candidate = self._candidate()
        (self.repo / "generated.test-output").write_text("normal ignored build artifact\n")
        driver.require_clean_candidate(self.repo, candidate)

    def test_changed_head_is_rejected_even_if_worktree_is_clean(self) -> None:
        candidate = self._candidate()
        (self.repo / "go.sum").write_text("fixture-dependency v1.0.2 h1:changed-head\n")
        changed = self._commit("Build modified candidate history")
        self.assertNotEqual(changed, candidate)
        self.assertEqual(self._git(self.repo, "status", "--porcelain"), "")
        with self.assertRaises(ValueError):
            driver.require_clean_candidate(self.repo, candidate)


class GenerationBoundaryFixture(GitFixture):
    def _remote_candidates(self, count: int, prefix: str = "dependency-updates/") -> None:
        updates = "".join(
            f"create refs/heads/{prefix}{index}-1/fixture {self.baseline}\n"
            for index in range(count)
        )
        subprocess.run(
            ["git", "-C", str(self.remote), "update-ref", "--stdin"],
            input=updates, text=True, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        )

    def test_unused_per_run_candidate_namespace_succeeds(self) -> None:
        driver.require_unused_candidate_namespace(self.repo, "dependency-updates/42-3/")

    def test_existing_candidate_namespace_cannot_be_reused(self) -> None:
        self.branch = "dependency-updates/42-3/fixture"
        self._candidate()
        with self.assertRaises(ValueError):
            driver.require_unused_candidate_namespace(self.repo, "dependency-updates/42-3/")

    def test_another_runs_candidate_does_not_collide(self) -> None:
        self.branch = "dependency-updates/42-2/fixture"
        self._candidate()
        driver.require_unused_candidate_namespace(self.repo, "dependency-updates/42-3/")

    def test_similar_prefix_does_not_collide_across_ref_component_boundary(self) -> None:
        self.branch = "dependency-updates/42-30/fixture"
        self._candidate()
        driver.require_unused_candidate_namespace(self.repo, "dependency-updates/42-3/")

    def test_current_remote_default_is_a_valid_generation_baseline(self) -> None:
        driver.require_generation_baseline(self.repo, self.baseline, self.policy)

    def test_advanced_remote_default_rejects_stale_generation_baseline(self) -> None:
        competitor = self._competing_commit()
        self._git(self.remote, "update-ref", "refs/heads/develop", competitor, self.baseline)
        with self.assertRaises(ValueError):
            driver.require_generation_baseline(self.repo, self.baseline, self.policy)
        self.assertEqual(self._remote_head(), competitor)

    def test_nondefault_commit_cannot_be_used_as_generation_baseline(self) -> None:
        competitor = self._competing_commit()
        with self.assertRaises(ValueError):
            driver.require_generation_baseline(self.repo, competitor, self.policy)
        self.assertEqual(self._remote_head(), self.baseline)

    def test_candidate_retention_below_limit_allows_generation(self) -> None:
        self.policy["candidateRetentionLimit"] = 32
        self._remote_candidates(31)
        driver.require_candidate_retention_capacity(self.repo, self.policy)

    def test_candidate_retention_at_limit_holds_generation_without_deletion(self) -> None:
        self.policy["candidateRetentionLimit"] = 32
        self._remote_candidates(32)
        before = self._git(self.repo, "ls-remote", "--heads", "origin")
        with self.assertRaises(ValueError):
            driver.require_candidate_retention_capacity(self.repo, self.policy)
        self.assertEqual(self._git(self.repo, "ls-remote", "--heads", "origin"), before)
        self.assertEqual(self._remote_head(), self.baseline)

    def test_unrelated_refs_do_not_consume_candidate_retention_capacity(self) -> None:
        self.policy["candidateRetentionLimit"] = 32
        self._remote_candidates(33, prefix="feature/")
        driver.require_candidate_retention_capacity(self.repo, self.policy)


class AppTokenFixture(unittest.TestCase):
    def setUp(self) -> None:
        self.environment = {
            "GITHUB_REPOSITORY": "fixture/repo",
            "DEPENDENCY_UPDATE_APP_ID": "200",
            "DEPENDENCY_UPDATE_APP_PRIVATE_KEY": "fixture writer key",
            "DEPENDENCY_UPDATE_READ_APP_ID": "100",
            "DEPENDENCY_UPDATE_READ_APP_PRIVATE_KEY": "fixture reader key",
        }
        self.reader_permissions = {"contents": "read", "metadata": "read"}
        self.writer_permissions = {
            "contents": "write", "metadata": "read", "issues": "write", "pull_requests": "read",
        }

    def _mint(self, write: bool, app_permissions=None, token_permissions=None):
        defaults = self.writer_permissions if write else self.reader_permissions
        authoritative_api = mock.Mock(side_effect=[
            {"permissions": defaults if app_permissions is None else app_permissions},
            {"id": 7},
            {"token": "fixture-scoped-token", "permissions": defaults if token_permissions is None else token_permissions},
        ])

        def sign(command, **kwargs):
            self.assertEqual(command[:4], ["openssl", "dgst", "-sha256", "-sign"])
            key = Path(command[4])
            self.assertEqual(key.stat().st_mode & 0o777, 0o600)
            self.assertEqual(key.read_text(), self.environment[
                "DEPENDENCY_UPDATE_APP_PRIVATE_KEY" if write else "DEPENDENCY_UPDATE_READ_APP_PRIVATE_KEY"
            ])
            self.assertIsInstance(kwargs["input"], bytes)
            return subprocess.CompletedProcess(command, 0, stdout=b"fixture-signature", stderr=b"")

        with mock.patch.dict(os.environ, self.environment):
            with mock.patch.object(driver, "api", authoritative_api):
                with mock.patch.object(subprocess, "run", side_effect=sign):
                    token = driver.app_token(write)
        return token, authoritative_api

    @staticmethod
    def _issuer(jwt: str) -> str:
        encoded = jwt.split(".")[1]
        return json.loads(base64.urlsafe_b64decode(encoded + "=" * (-len(encoded) % 4)))["iss"]

    def test_reader_uses_independent_read_app_and_explicit_read_permissions(self) -> None:
        token, authoritative_api = self._mint(False)
        self.assertEqual(token, "fixture-scoped-token")
        jwt = authoritative_api.call_args_list[0].args[1]
        self.assertEqual(self._issuer(jwt), "100")
        self.assertEqual(authoritative_api.call_args_list, [
            mock.call("/app", jwt),
            mock.call("/repos/fixture/repo/installation", jwt),
            mock.call("/app/installations/7/access_tokens", jwt, {"permissions": self.reader_permissions}),
        ])

    def test_writer_is_scoped_to_current_repository_without_pr_write(self) -> None:
        token, authoritative_api = self._mint(True)
        self.assertEqual(token, "fixture-scoped-token")
        jwt = authoritative_api.call_args_list[0].args[1]
        self.assertEqual(self._issuer(jwt), "200")
        self.assertEqual(authoritative_api.call_args_list[-1], mock.call(
            "/app/installations/7/access_tokens", jwt,
            {"permissions": self.writer_permissions, "repositories": ["repo"]},
        ))

    def test_missing_explicit_app_configuration_fails_before_signing_or_api(self) -> None:
        for write, name in ((True, "DEPENDENCY_UPDATE_APP"), (False, "DEPENDENCY_UPDATE_READ_APP")):
            for missing in ("_ID", "_PRIVATE_KEY"):
                with self.subTest(write=write, missing=missing):
                    environment = {**self.environment, name + missing: ""}
                    with mock.patch.dict(os.environ, environment):
                        with mock.patch.object(driver, "api") as authoritative_api:
                            with mock.patch.object(subprocess, "run") as signer:
                                with self.assertRaisesRegex(ValueError, name):
                                    driver.app_token(write)
                                authoritative_api.assert_not_called()
                                signer.assert_not_called()

    def test_reader_app_with_any_global_write_permission_is_rejected(self) -> None:
        for permission in ("contents", "issues", "checks", "deployments", "pull_requests"):
            with self.subTest(permission=permission):
                with self.assertRaises(ValueError):
                    self._mint(False, app_permissions={**self.reader_permissions, permission: "write"})

    def test_reader_app_must_have_globally_read_only_contents_access(self) -> None:
        for permissions in ({}, {"metadata": "read"}, {"contents": "none", "metadata": "read"}):
            with self.subTest(permissions=permissions):
                with self.assertRaises(ValueError):
                    self._mint(False, app_permissions=permissions)

    def test_writer_app_with_global_pr_write_permission_is_rejected(self) -> None:
        with self.assertRaises(ValueError):
            self._mint(True, app_permissions={**self.writer_permissions, "pull_requests": "write"})

    def test_returned_reader_token_with_write_permission_is_rejected(self) -> None:
        for permission in ("contents", "issues", "checks"):
            with self.subTest(permission=permission):
                with self.assertRaises(ValueError):
                    self._mint(False, token_permissions={**self.reader_permissions, permission: "write"})

    def test_returned_writer_token_with_pr_write_permission_is_rejected(self) -> None:
        with self.assertRaises(ValueError):
            self._mint(True, token_permissions={**self.writer_permissions, "pull_requests": "write"})


class PrivateContainerConfigurationFixture(unittest.TestCase):
    def test_configuration_becomes_private_before_noninteractive_owner_change(self) -> None:
        with tempfile.TemporaryDirectory(prefix="private-container-config-") as directory:
            path = Path(directory) / "config.json"
            path.write_text('{"token":"fixture-token"}\n')
            path.chmod(0o644)

            def change_owner(command, **kwargs):
                self.assertEqual(path.stat().st_mode & 0o777, 0o600)
                self.assertEqual(command, ["sudo", "-n", "chown", "12021:0", str(path)])
                self.assertEqual(kwargs, {"check": True})
                return subprocess.CompletedProcess(command, 0)

            with mock.patch.object(subprocess, "run", side_effect=change_owner) as owner_change:
                driver.prepare_private_container_config(path)
            owner_change.assert_called_once()
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            self.assertEqual(path.read_text(), '{"token":"fixture-token"}\n')

    def test_owner_change_failure_propagates_without_broadening_permissions(self) -> None:
        with tempfile.TemporaryDirectory(prefix="private-container-config-") as directory:
            path = Path(directory) / "config.json"
            path.write_text("fixture private configuration\n")
            path.chmod(0o644)
            error = subprocess.CalledProcessError(1, ["sudo", "-n", "chown"])
            with mock.patch.object(subprocess, "run", side_effect=error):
                with self.assertRaises(subprocess.CalledProcessError) as raised:
                    driver.prepare_private_container_config(path)
            self.assertIs(raised.exception, error)
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)


class ContainerGenerationFixture(GitFixture):
    def test_generator_preserves_official_image_user_and_private_readonly_mount(self) -> None:
        policy = {**self.policy, "candidateRetentionLimit": 32,
                  "renovateImage": "ghcr.io/renovatebot/renovate:44.132.2-full"}
        policy_path = self.repo / ".github" / "dependency-updates" / "policy.json"
        policy_path.parent.mkdir(parents=True)
        policy_path.write_text(json.dumps(policy))
        (self.repo / "renovate.json").write_text('{"packageRules":[]}\n')
        self.baseline = self._commit("Trusted fixture generator configuration")
        self._git(self.repo, "push", "origin", "develop")
        runner_temp = self.root / "runner-temp"
        runner_temp.mkdir()
        environment = {
            "GITHUB_SHA": self.baseline, "GITHUB_RUN_ID": "42", "GITHUB_RUN_ATTEMPT": "3",
            "RUNNER_TEMP": str(runner_temp), "GITHUB_OUTPUT": str(self.root / "outputs"),
            "DEPENDENCY_READ_TOKEN": "fixture-reader-token", "RENOVATE_TOKEN": "fixture-writer-token",
        }
        original_run = subprocess.run
        docker_commands = []
        generated_configuration = []

        def run_command(command, *arguments, **kwargs):
            if command[0] == "sudo":
                self.assertEqual(command[:4], ["sudo", "-n", "chown", "12021:0"])
                return subprocess.CompletedProcess(command, 0)
            if command[0] == "docker":
                docker_commands.append(command)
                self.assertEqual((runner_temp / "renovate" / "config.json").stat().st_mode & 0o777, 0o600)
                generated_configuration.append(json.loads((runner_temp / "renovate" / "config.json").read_text()))
                return subprocess.CompletedProcess(command, 0)
            return original_run(command, *arguments, **kwargs)

        original_cwd = Path.cwd()
        try:
            os.chdir(self.repo)
            with mock.patch.dict(os.environ, environment):
                with mock.patch.object(sys, "argv", ["driver.py", "generate"]):
                    with mock.patch.object(subprocess, "run", side_effect=run_command):
                        driver.main()
        finally:
            os.chdir(original_cwd)
        self.assertEqual(len(docker_commands), 2)
        self.assertIn("GOPRIVATE=github.com/axiom-studio/*", docker_commands[1])
        for config in generated_configuration:
            api_read_rules = [rule for rule in config["hostRules"] if rule.get("hostType") == "github" and rule.get("matchHost") == "api.github.com"]
            self.assertEqual(len(api_read_rules), 1)
            self.assertIs(api_read_rules[0]["readOnly"], True)
            self.assertEqual(api_read_rules[0]["token"], environment["DEPENDENCY_READ_TOKEN"])
        for command in docker_commands:
            self.assertEqual(command[:3], ["docker", "run", "--rm"])
            self.assertIn(policy["renovateImage"], command)
            self.assertNotIn("--user", command)
            self.assertNotIn("--privileged", command)
            self.assertEqual(command[command.index("--volume") + 1], str(runner_temp / "renovate") + ":/config:ro")
        self.assertFalse((runner_temp / "renovate" / "config.json").exists())


class WireToolPolicyFixture(GitFixture):
    def setUp(self) -> None:
        root = Path(self._git(Path.cwd(), "rev-parse", "--show-toplevel"))
        policy = None
        for checkout in (root, root.parent / "cortex"):
            path = checkout / ".github/dependency-updates/policy.json"
            if path.is_file():
                candidate = json.loads(path.read_text())
                if candidate["repository"] == "axiom-studio/cortex":
                    policy = candidate
                    break
        if policy is None:
            self.skipTest("Cortex policy is not present in this isolated repository job")
        super().setUp()
        self.policy = policy

    def test_locked_wire_tool_manifest_and_checksums_are_dependency_inputs(self) -> None:
        changes = {
            ".github/dependency-updates/wire-tool/go.mod": "module fixture/wire-tool\n\ngo 1.26.0\n",
            ".github/dependency-updates/wire-tool/go.sum": "fixture tool checksum\n",
        }
        receipt = self._receipt(self._candidate(changes))
        self.assertEqual(receipt["changedFiles"], sorted(changes))

    def test_wire_tool_source_cannot_change_through_dependency_candidates(self) -> None:
        candidate = self._candidate({".github/dependency-updates/wire-tool/tools.go": "package tools\n"})
        with self.assertRaisesRegex(ValueError, "forbidden path"):
            self._receipt(candidate)


class SourceConfigurationFixture(unittest.TestCase):
    """Check the committed workflow's fixed mapping layout without dependencies.

    These structural checks supplement GitHub's YAML parser. They intentionally
    reject an unexpected job layout instead of interpreting arbitrary YAML.
    """

    DEFAULT_BRANCHES = {
        "axiom-studio/cortex": "develop",
        "axiom-studio/openseal": "develop",
        "axiom-studio/axiomcloud": "main",
        "axiom-studio/skills": "main",
    }

    def setUp(self) -> None:
        root = Path(subprocess.run(
            ["git", "rev-parse", "--show-toplevel"], check=True, text=True,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        ).stdout.strip())
        policy_path = Path(".github/dependency-updates/policy.json")
        self.assertTrue((root / policy_path).is_file(), "Current checkout must contain the dependency policy")
        self.checkouts = [root]
        for repository in self.DEFAULT_BRANCHES:
            sibling = root.parent / repository.split("/")[1]
            if sibling != root and (sibling / policy_path).exists():
                self.checkouts.append(sibling)

    @staticmethod
    def _policy(checkout: Path) -> dict:
        return json.loads((checkout / ".github/dependency-updates/policy.json").read_text())

    @staticmethod
    def _workflow(checkout: Path) -> str:
        workflow = checkout / driver.WORKFLOW
        if not workflow.is_file():
            workflow = checkout / ".github/dependency-updates/workflow.disabled.yml"
        return workflow.read_text()

    def _jobs(self, checkout: Path) -> dict[str, str]:
        workflow = self._workflow(checkout)
        self.assertIn("\njobs:\n", workflow)
        section = workflow.split("\njobs:\n", 1)[1]
        starts = list(re.finditer(r"(?m)^  ([a-z][a-z0-9-]*):\s*$", section))
        jobs = {
            match.group(1): section[match.end():starts[index + 1].start() if index + 1 < len(starts) else len(section)]
            for index, match in enumerate(starts)
        }
        self.assertEqual(set(jobs), {"generate", "validate", "promote", "default-ci"})
        return jobs

    def _job_value(self, block: str, key: str) -> str:
        match = re.search(r"(?m)^    " + re.escape(key) + r":\s*([^\n]+)$", block)
        self.assertIsNotNone(match, f"Missing job-level {key!r}")
        return match.group(1).strip()

    def test_shared_driver_and_dependency_configuration_match(self) -> None:
        reference = self.checkouts[0]
        expected_driver = (reference / ".github/dependency-updates/driver.py").read_bytes()
        expected_configuration = json.loads((reference / "renovate.json").read_text())
        for checkout in self.checkouts:
            with self.subTest(checkout=checkout.name):
                self.assertEqual((checkout / ".github/dependency-updates/driver.py").read_bytes(), expected_driver)
                self.assertEqual(json.loads((checkout / "renovate.json").read_text()), expected_configuration)

    def test_default_branches_match_policy_and_push_trigger(self) -> None:
        for checkout in self.checkouts:
            with self.subTest(checkout=checkout.name):
                policy = self._policy(checkout)
                expected = self.DEFAULT_BRANCHES[policy["repository"]]
                self.assertEqual(policy["defaultBranch"], expected)
                self.assertEqual(policy["candidateRetentionLimit"], 32)
                self.assertRegex(self._workflow(checkout), rf"(?m)^  push:\n    branches: \[{re.escape(expected)}\]\s*$")
                self.assertTrue((checkout / policy["goModule"]).is_file())

    def test_all_jobs_use_hosted_runners_and_bounded_timeouts(self) -> None:
        for checkout in self.checkouts:
            for name, block in self._jobs(checkout).items():
                with self.subTest(checkout=checkout.name, job=name):
                    self.assertEqual(self._job_value(block, "runs-on"), "ubuntu-24.04")
                    minutes = int(self._job_value(block, "timeout-minutes"))
                    self.assertGreater(minutes, 0)
                    self.assertLessEqual(minutes, 90)

    def test_workflows_never_grant_pr_write_and_validation_has_no_writer_key(self) -> None:
        for checkout in self.checkouts:
            with self.subTest(checkout=checkout.name):
                workflow = self._workflow(checkout)
                self.assertNotRegex(workflow, r"(?m)^\s*pull-requests:\s*write\s*$")
                self.assertNotRegex(workflow, r"(?m)^\s*permissions:\s*write-all\s*$")
                self.assertRegex(workflow, r"(?m)^permissions:\n  contents: read\s*$")
                for name in ("validate", "default-ci"):
                    block = self._jobs(checkout)[name]
                    self.assertNotIn("DEPENDENCY_UPDATE_APP_PRIVATE_KEY", block)
                    self.assertNotIn("DEPENDENCY_UPDATE_APP_ID", block)
                    self.assertNotIn("token-candidate", block)
                    self.assertNotRegex(block, r"(?m)^\s*contents:\s*write\s*$")

    def test_exact_validation_job_name_matches_authoritative_check_and_policy(self) -> None:
        for checkout in self.checkouts:
            with self.subTest(checkout=checkout.name):
                policy = self._policy(checkout)
                self.assertEqual(len(policy["checks"]), 1)
                self.assertEqual(policy["checks"][0]["name"], driver.CHECK_JOB)
                self.assertEqual(self._job_value(self._jobs(checkout)["validate"], "name"), driver.CHECK_JOB)
                script = policy["checks"][0]["script"]
                self.assertTrue((checkout / script).is_file())
                validate = self._jobs(checkout)["validate"]
                self.assertIn("trusted-dependency-updates/checks.sh", validate)
                self.assertIn(" clean --candidate", validate)
                self.assertLess(validate.index("trusted-dependency-updates/checks.sh"), validate.index(" clean --candidate"))
                self.assertIn("--receipt \"$RUNNER_TEMP/receipt.json\"", validate)
                read_token = validate.index(" token-read")
                vendor_proof = validate.index(" verify-candidate-vendor ")
                tests = validate.index("trusted-dependency-updates/checks.sh")
                self.assertLess(read_token, vendor_proof)
                self.assertLess(vendor_proof, tests)

    def test_workflow_cleanup_is_a_separate_step_after_successful_promotion(self) -> None:
        for checkout in self.checkouts:
            with self.subTest(checkout=checkout.name):
                promote = self._jobs(checkout)["promote"]
                promotion = promote.index("driver.py promote ")
                cleanup = promote.index("driver.py cleanup ")
                steps_between = promote[promotion:cleanup]
                self.assertIn("\n      - name:", steps_between)
                self.assertIn("        continue-on-error: true", steps_between)
                self.assertIn('--receipt "$RUNNER_TEMP/promoted-receipt.json"', promote[cleanup:])

    def test_default_push_ci_runs_real_repository_tests(self) -> None:
        test_commands = {
            "axiom-studio/cortex": "make test-unit",
            "axiom-studio/openseal": "make test",
            "axiom-studio/axiomcloud": "make tests",
            "axiom-studio/skills": "go test",
        }
        for checkout in self.checkouts:
            with self.subTest(checkout=checkout.name):
                policy = self._policy(checkout)
                block = self._jobs(checkout)["default-ci"]
                self.assertEqual(self._job_value(block, "if"), "github.event_name == 'push'")
                self.assertIn(policy["checks"][0]["script"], block)
                checks = (checkout / policy["checks"][0]["script"]).read_text()
                self.assertIn("set -euo pipefail", checks)
                self.assertIn(test_commands[policy["repository"]], checks)

    def test_sdk_prepares_supported_tauri_sidecar_before_locked_cargo_check(self) -> None:
        sdk_checkouts = [checkout for checkout in self.checkouts if self._policy(checkout)["repository"] == "axiom-studio/openseal"]
        if not sdk_checkouts:
            self.skipTest("OpenSeal SDK checkout is not present in this isolated repository job")
        for checkout in sdk_checkouts:
            with self.subTest(checkout=checkout.name):
                script = (checkout / ".github/dependency-updates/checks.sh").read_text()
                commands = [line.strip() for line in script.splitlines() if line.strip() and not line.lstrip().startswith("#")]
                recipe = [
                    "make build",
                    "cp -f THIRD_PARTY_NOTICES desktop/src-tauri/THIRD_PARTY_NOTICES",
                    "corepack pnpm --dir desktop prepare:daemon",
                    "cargo check --locked --manifest-path desktop/src-tauri/Cargo.toml",
                ]
                positions = [commands.index(command) for command in recipe]
                self.assertEqual(positions, sorted(positions))
                package = json.loads((checkout / "desktop/package.json").read_text())
                self.assertEqual(package["scripts"]["prepare:daemon"], "node scripts/prepare-daemon.mjs")
                self.assertTrue((checkout / "desktop/scripts/prepare-daemon.mjs").is_file())

    def test_cortex_ci_and_image_use_the_same_locked_wire_tool(self) -> None:
        checkouts = [checkout for checkout in self.checkouts if self._policy(checkout)["repository"] == "axiom-studio/cortex"]
        if not checkouts:
            self.skipTest("Cortex checkout is not present in this isolated repository job")
        for checkout in checkouts:
            checks = (checkout / ".github/dependency-updates/checks.sh").read_text()
            dockerfile = (checkout / "Dockerfile").read_text()
            tool = ".github/dependency-updates/wire-tool"
            self.assertIn("go build -C " + tool + " -mod=readonly", checks)
            self.assertIn("COPY " + tool + "/ ./", dockerfile)
            self.assertIn("go build -mod=readonly", dockerfile)
            self.assertIn("github.com/google/wire/cmd/wire", checks)
            self.assertIn("github.com/google/wire/cmd/wire", dockerfile)
            self.assertNotIn("wire/cmd/wire@latest", dockerfile + checks)
            self.assertTrue((checkout / tool / "go.sum").is_file())
            self.assertRegex((checkout / tool / "go.mod").read_text(), r"(?m)^require github\.com/google/wire v\d+\.\d+\.\d+$")
            exceptions = {line.strip() for line in (checkout / ".dockerignore").read_text().splitlines() if line.startswith("!.github")}
            self.assertEqual(exceptions, {
                "!.github/", "!.github/dependency-updates/", "!" + tool + "/",
                "!" + tool + "/go.mod", "!" + tool + "/go.sum", "!" + tool + "/tools.go",
            })

    def test_cortex_registers_declared_helm_repositories_before_building_dependencies(self) -> None:
        checkouts = [checkout for checkout in self.checkouts if self._policy(checkout)["repository"] == "axiom-studio/cortex"]
        if not checkouts:
            self.skipTest("Cortex checkout is not present in this isolated repository job")
        for checkout in checkouts:
            checks = (checkout / ".github/dependency-updates/checks.sh").read_text()
            listing = checks.index("helm dependency list charts/axiom")
            registration = checks.index("helm repo add ")
            build = checks.index("helm dependency build charts/axiom")
            self.assertLess(listing, registration)
            self.assertLess(registration, build)

    def test_cortex_legacy_ci_remains_hosted_bounded_and_runs_existing_checks(self) -> None:
        checkouts = [checkout for checkout in self.checkouts if self._policy(checkout)["repository"] == "axiom-studio/cortex"]
        if not checkouts:
            self.skipTest("Cortex checkout is not present in this isolated repository job")
        for checkout in checkouts:
            workflow = (checkout / ".github/workflows/ci.yml").read_text()
            self.assertRegex(workflow, r"(?m)^    runs-on: ubuntu-24\.04$")
            self.assertRegex(workflow, r"(?m)^    timeout-minutes: 70$")
            self.assertRegex(workflow, r"(?m)^      GOMAXPROCS: 2$")
            self.assertRegex(workflow, r"(?m)^      GOFLAGS: -p=2$")
            self.assertNotIn("self-hosted", workflow)
            for check in ("go build -mod=vendor ./...", "make check-encryption-key-loading", "make test-unit"):
                self.assertIn(check, workflow)
            self.assertIn("go build -C .github/dependency-updates/wire-tool -mod=readonly", workflow)
            self.assertIn('GOFLAGS="$GOFLAGS -mod=vendor -p=2"', workflow)

    def test_major_and_pre_one_minor_updates_require_review(self) -> None:
        for checkout in self.checkouts:
            with self.subTest(checkout=checkout.name):
                configuration = json.loads((checkout / "renovate.json").read_text())
                rules = configuration["packageRules"]
                self.assertTrue(any(
                    rule.get("enabled") is False and "major" in rule.get("matchUpdateTypes", [])
                    for rule in rules
                ))
                zero_minor_rules = [
                    rule for rule in rules if rule.get("enabled") is False
                    and "minor" in rule.get("matchUpdateTypes", []) and "matchCurrentVersion" in rule
                ]
                self.assertTrue(zero_minor_rules)
                for version in ("0.3.0", "v0.9.2"):
                    self.assertTrue(any(
                        re.search(rule["matchCurrentVersion"][1:-1], version)
                        for rule in zero_minor_rules
                    ))
                self.assertFalse(any(
                    re.search(rule["matchCurrentVersion"][1:-1], "1.0.0")
                    for rule in zero_minor_rules
                ))


if __name__ == "__main__":
    unittest.main()
