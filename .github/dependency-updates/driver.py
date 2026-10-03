#!/usr/bin/env python3
"""Trusted CI orchestration; ecosystem version resolution belongs to Renovate."""
from __future__ import annotations

import argparse
import base64
import fnmatch
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import time
import urllib.request


SHA = re.compile(r"^[0-9a-f]{40}$")
WORKFLOW = ".github/workflows/dependency-updates.yml"
CHECK_JOB = "Validate exact candidate"


def git(repo: Path, *args: str) -> str:
    return subprocess.run(["git", "-C", str(repo), *args], check=True,
                          text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE).stdout.strip()


def digest(value: object) -> str:
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def require_unused_candidate_namespace(repo: Path, prefix: str) -> None:
    if not prefix.startswith("dependency-updates/") or not re.fullmatch(r"dependency-updates/[0-9]+-[0-9]+/", prefix):
        raise ValueError("candidate namespace must identify this workflow run and attempt")
    if git(repo, "ls-remote", "--heads", "origin", "refs/heads/" + prefix + "*"):
        raise ValueError("candidate namespace already exists; use a fresh workflow attempt")


def require_generation_baseline(repo: Path, baseline: str, policy: dict) -> None:
    lines = git(repo, "ls-remote", "--heads", "origin", "refs/heads/" + policy["defaultBranch"]).splitlines()
    if len(lines) != 1 or lines[0].split()[0] != baseline:
        raise ValueError("default branch advanced before generation; retry from its fresh baseline")


def require_candidate_retention_capacity(repo: Path, policy: dict) -> None:
    limit = policy.get("candidateRetentionLimit")
    if not isinstance(limit, int) or isinstance(limit, bool) or not 1 <= limit <= 100:
        raise ValueError("candidate retention requires an explicit bounded ref limit")
    refs = git(repo, "ls-remote", "--heads", "origin", "refs/heads/" + policy["candidatePrefix"] + "*").splitlines()
    if len(refs) >= limit:
        raise ValueError("candidate retention limit reached; audit receipts and manually clean unchanged owned candidates")


def inspect_candidate(repo: Path, baseline: str, candidate: str, policy: dict, branch: str) -> dict:
    if not SHA.fullmatch(baseline) or not SHA.fullmatch(candidate):
        raise ValueError("exact full commit SHAs are required")
    prefix = policy["candidatePrefix"]
    if not branch.startswith(prefix) or branch == policy["defaultBranch"]:
        raise ValueError("candidate must use the reserved dependency branch prefix")
    if subprocess.run(["git", "check-ref-format", "refs/heads/" + branch], capture_output=True).returncode:
        raise ValueError("invalid candidate branch")
    parents = git(repo, "rev-list", "--parents", "-n", "1", candidate).split()
    if parents != [candidate, baseline]:
        raise ValueError("candidate must have exactly one commit on the tested baseline")
    raw = subprocess.run(["git", "-C", str(repo), "diff", "--raw", "-z", "--no-renames", baseline, candidate],
                         check=True, stdout=subprocess.PIPE).stdout.split(b"\0")
    changed = []
    for index in range(0, len(raw) - 1, 2):
        metadata = raw[index].decode().split()
        path = raw[index + 1].decode("utf-8", "strict")
        if not any(fnmatch.fnmatchcase(path, pattern) for pattern in policy["allowedPaths"]):
            raise ValueError("candidate changes forbidden path: " + path)
        old_mode, new_mode = metadata[0][1:], metadata[1]
        if new_mode not in {"000000", "100644"} or old_mode not in {"000000", "100644"}:
            raise ValueError("candidate changes a symlink, executable, or file type: " + path)
        changed.append(path)
    if not changed:
        raise ValueError("empty dependency candidate")
    return {"schemaVersion": 1, "repository": policy["repository"],
            "defaultBranch": policy["defaultBranch"], "baseline": baseline,
            "candidate": candidate, "branch": branch, "policyDigest": digest(policy),
            "changedFiles": sorted(changed)}


def promote_candidate(repo: Path, receipt: dict, policy: dict, verify_checks, remote: str = "origin") -> str:
    # A receipt's self-reported success is never execution evidence.
    verify_checks(receipt)
    expected = inspect_candidate(repo, receipt["baseline"], receipt["candidate"], policy, receipt["branch"])
    if receipt != expected:
        raise ValueError("candidate receipt or trusted policy changed")
    default = policy["defaultBranch"]
    git(repo, "fetch", "--no-tags", remote, "refs/heads/" + default, "refs/heads/" + receipt["branch"])
    refs = dict(line.split()[::-1] for line in git(repo, "ls-remote", "--heads", remote,
                "refs/heads/" + default, "refs/heads/" + receipt["branch"]).splitlines())
    if refs.get("refs/heads/" + default) != receipt["baseline"]:
        raise ValueError("default branch advanced; regenerate and retest from its fresh baseline")
    if refs.get("refs/heads/" + receipt["branch"]) != receipt["candidate"]:
        raise ValueError("remote candidate changed or disappeared after validation")
    # Git's server-side fast-forward check protects a concurrent default advance.
    # No force, lease-force, API overwrite, rebase, or untested merge commit.
    git(repo, "push", remote, receipt["candidate"] + ":refs/heads/" + default)
    return receipt["candidate"]


def cleanup_promoted_candidate(repo: Path, receipt: dict, policy: dict, remote: str = "origin") -> None:
    prefix = policy["candidatePrefix"] + os.environ["GITHUB_RUN_ID"] + "-" + os.environ["GITHUB_RUN_ATTEMPT"] + "/"
    branch = receipt["branch"]
    if not branch.startswith(prefix) or branch == policy["defaultBranch"]:
        raise ValueError("cleanup is limited to the just-promoted workflow candidate")
    ref = "refs/heads/" + branch
    # This lease applies only to deletion of this run's exact candidate. A new
    # user commit or replacement ref causes server rejection rather than loss.
    git(repo, "push", remote, "--force-with-lease=" + ref + ":" + receipt["candidate"], ":" + ref)


def require_clean_candidate(repo: Path, candidate: str) -> None:
    if git(repo, "rev-parse", "HEAD") != candidate:
        raise ValueError("candidate checkout changed while checks ran")
    if git(repo, "status", "--porcelain", "--untracked-files=all"):
        raise ValueError("checks changed tracked dependency/build inputs; regenerate and retest the committed candidate")


def api(path: str, token: str, body=None, method=None):
    if not path.startswith("/"):
        raise ValueError("GitHub API path must be relative")
    request = urllib.request.Request("https://api.github.com" + path,
        data=None if body is None else json.dumps(body).encode(), method=method,
        headers={"Authorization": "Bearer " + token, "Accept": "application/vnd.github+json",
                 "X-GitHub-Api-Version": "2022-11-28", "User-Agent": "axiom-dependency-updates"})
    with urllib.request.urlopen(request, timeout=30) as response:
        return json.load(response)


def verify_workflow_checks(receipt: dict) -> None:
    repository = os.environ["GITHUB_REPOSITORY"]
    run_id = os.environ["GITHUB_RUN_ID"]
    attempt = int(os.environ["GITHUB_RUN_ATTEMPT"])
    if repository != receipt["repository"]:
        raise ValueError("receipt belongs to another repository")
    token = os.environ["GITHUB_TOKEN"]
    run = api(f"/repos/{repository}/actions/runs/{run_id}", token)
    if (run["head_sha"] != receipt["baseline"] or run["run_attempt"] != attempt or
        run["path"].split("@")[0] != WORKFLOW or run["event"] not in {"schedule", "workflow_dispatch"}):
        raise ValueError("validation did not run from the exact trusted baseline workflow")
    jobs = api(f"/repos/{repository}/actions/runs/{run_id}/attempts/{attempt}/jobs?per_page=100", token)
    matches = [job for job in jobs["jobs"] if job["name"] == CHECK_JOB]
    if len(matches) != 1 or matches[0]["conclusion"] != "success" or matches[0]["status"] != "completed":
        raise ValueError("exact candidate validation has no authoritative successful job")


def output(**values) -> None:
    with open(os.environ["GITHUB_OUTPUT"], "a", encoding="utf-8") as handle:
        for name, value in values.items():
            if "\n" in str(value) or "\r" in str(value):
                raise ValueError("invalid output value")
            handle.write(f"{name}={value}\n")


def app_token(write: bool) -> str:
    name = "DEPENDENCY_UPDATE_APP" if write else "DEPENDENCY_UPDATE_READ_APP"
    app_id = os.environ.get(name + "_ID", "")
    private_key = os.environ.get(name + "_PRIVATE_KEY", "")
    if not app_id or not private_key:
        raise ValueError("configure " + name + "_ID and " + name + "_PRIVATE_KEY; no existing secret is reused")
    encode = lambda data: base64.urlsafe_b64encode(data).rstrip(b"=")
    now = int(time.time())
    unsigned = encode(b'{"alg":"RS256","typ":"JWT"}') + b"." + encode(json.dumps(
        {"iat": now - 60, "exp": now + 540, "iss": app_id}, separators=(",", ":")).encode())
    with tempfile.TemporaryDirectory() as directory:
        key = Path(directory) / "key.pem"
        key.write_text(private_key)
        key.chmod(0o600)
        signature = subprocess.run(["openssl", "dgst", "-sha256", "-sign", str(key)],
            input=unsigned, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE).stdout
    jwt = (unsigned + b"." + encode(signature)).decode()
    app_permissions = api("/app", jwt).get("permissions", {})
    if not isinstance(app_permissions, dict) or app_permissions.get("pull_requests") == "write":
        raise ValueError("dependency Apps must not have pull-request write permission")
    if write and any(value == "write" and key not in {"contents", "issues"}
                     for key, value in app_permissions.items()):
        raise ValueError("writer App write permissions must be limited to contents and issues")
    if not write and (app_permissions.get("contents") != "read" or "write" in app_permissions.values()):
        raise ValueError("validation requires an independently read-only App, not the writer App key")
    repository = os.environ["GITHUB_REPOSITORY"]
    installation = api(f"/repos/{repository}/installation", jwt)
    permissions = {"contents": "write" if write else "read", "metadata": "read"}
    body = {"permissions": permissions}
    if write:
        # Candidate generation may manage its explicit dashboard; PR writes are absent.
        permissions.update({"issues": "write", "pull_requests": "read"})
        body["repositories"] = [repository.split("/", 1)[1]]
    result = api(f"/app/installations/{installation['id']}/access_tokens", jwt, body)
    if result.get("permissions", {}).get("pull_requests") == "write":
        raise ValueError("candidate App token must not have pull-request write permission")
    if not write and "write" in result.get("permissions", {}).values():
        raise ValueError("validation token must be read-only")
    return result["token"]


def git_auth(token: str) -> dict:
    environment = os.environ.copy()
    encoded = base64.b64encode(("x-access-token:" + token).encode()).decode()
    environment.update(GIT_CONFIG_COUNT="1", GIT_CONFIG_KEY_0="http.https://github.com/.extraHeader",
                       GIT_CONFIG_VALUE_0="AUTHORIZATION: basic " + encoded)
    return environment


def canonical_vendor(repo: Path, baseline: str) -> bool:
    modules = [path for path in git(repo, "ls-files", "go.mod", "**/go.mod").splitlines()
               if not {"vendor", "node_modules"}.intersection(Path(path).parts)]
    relevant = [path for path in modules if (repo / Path(path).parent / "vendor").is_dir()]
    if not relevant:
        return True
    with tempfile.TemporaryDirectory() as directory:
        tree = Path(directory) / "source"
        git(repo, "worktree", "add", "--detach", str(tree), baseline)
        try:
            for module in relevant:
                root = tree / Path(module).parent
                environment = {key: value for key, value in os.environ.items()
                               if key != "RENOVATE_TOKEN" and not key.startswith("DEPENDENCY_UPDATE_")}
                subprocess.run(["go", "mod", "vendor"], cwd=root, env=environment, check=True)
                vendor = str(Path(module).parent / "vendor")
                git(tree, "add", "-A", "--", vendor)
                if subprocess.run(["git", "-C", str(tree), "diff", "--cached", "--quiet", "--", vendor]).returncode:
                    print("Go dependency updates held: checked-in vendor differs from canonical generation for " + module)
                    return False
            return True
        finally:
            git(repo, "worktree", "remove", "--force", str(tree))


def install_tools(repo: Path, policy: dict) -> None:
    # Bootstrap official toolchains, not dependency version resolution.
    version = re.search(r"^go (\d+\.\d+\.\d+)\s*$", (repo / policy["goModule"]).read_text(), re.M)
    if not version:
        raise ValueError("root go.mod must declare a full stable toolchain version")
    root = Path(os.environ["RUNNER_TEMP"]) / "dependency-toolchains"
    root.mkdir(parents=True, exist_ok=True)
    go_archive = "go" + version[1] + ".linux-amd64.tar.gz"
    with urllib.request.urlopen("https://go.dev/dl/?mode=json&include=all", timeout=30) as response:
        go_releases = json.load(response)
    go_files = [item for release in go_releases for item in release["files"] if item["filename"] == go_archive]
    if len(go_files) != 1:
        raise ValueError("root Go toolchain has no unique published checksum")
    with urllib.request.urlopen("https://nodejs.org/dist/latest-v24.x/SHASUMS256.txt", timeout=30) as response:
        node_sums = response.read().decode()
    match = re.search(r"^([0-9a-f]{64})\s+(node-v24\.\d+\.\d+-linux-x64\.tar\.xz)$", node_sums, re.M)
    if not match:
        raise ValueError("official Node 24 release checksum is unavailable")
    download_verified("https://go.dev/dl/" + go_archive, root / go_archive, go_files[0]["sha256"])
    subprocess.run(["tar", "-xf", go_archive], cwd=root, check=True)
    download_verified("https://nodejs.org/dist/latest-v24.x/" + match[2], root / match[2], match[1])
    subprocess.run(["tar", "-xf", match[2]], cwd=root, check=True)
    with open(os.environ["GITHUB_PATH"], "a") as handle:
        handle.write(str(root / "go/bin") + "\n" + str(root / match[2][:-7] / "bin") + "\n")
    with open(os.environ["GITHUB_ENV"], "a") as handle:
        handle.write("GOTOOLCHAIN=local\nGOPRIVATE=github.com/axiom-studio/*\n")


def download_verified(url: str, destination: Path, expected: str) -> None:
    if not re.fullmatch(r"[0-9a-f]{64}", expected):
        raise ValueError("invalid official archive checksum")
    checksum = hashlib.sha256()
    with urllib.request.urlopen(url, timeout=60) as response, destination.open("wb") as handle:
        while chunk := response.read(1024 * 1024):
            checksum.update(chunk)
            handle.write(chunk)
    if checksum.hexdigest() != expected:
        raise ValueError("official toolchain checksum mismatch")


def prepare_private_container_config(path: Path) -> None:
    # The pinned official image runs as its non-root UID 12021. Keep the token
    # unreadable to other users rather than broadening the file to mode 0644.
    path.chmod(0o600)
    subprocess.run(["sudo", "-n", "chown", "12021:0", str(path)], check=True)


def verify_candidate_vendor(repo: Path, baseline: str, candidate: str, policy: dict, branch: str, canonical: str) -> None:
    require_clean_candidate(repo, candidate)
    receipt = inspect_candidate(repo, baseline, candidate, policy, branch)
    enforce_vendor_hold(receipt, policy, canonical)
    changed_go = any(Path(path).name in {"go.mod", "go.sum"} or "vendor" in Path(path).parts
                     for path in receipt["changedFiles"])
    if policy.get("canonicalVendorRequired") and changed_go and not canonical_vendor(repo, candidate):
        raise ValueError("candidate vendor bytes differ from canonical generation of its exact module inputs")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("command", choices=["token-read", "token-candidate", "tools", "generate", "inspect", "clean", "verify-candidate-vendor", "promote", "cleanup"])
    parser.add_argument("--policy", default=".github/dependency-updates/policy.json")
    parser.add_argument("--baseline")
    parser.add_argument("--candidate")
    parser.add_argument("--branch")
    parser.add_argument("--receipt", default="receipt.json")
    parser.add_argument("--canonical-vendor", choices=["true", "false"])
    args = parser.parse_args()
    repo = Path.cwd()
    policy = json.loads(Path(args.policy).read_text())
    if args.command.startswith("token-"):
        token = app_token(args.command == "token-candidate")
        print("::add-mask::" + token)
        output(token=token)
    elif args.command == "tools":
        install_tools(repo, policy)
    elif args.command == "generate":
        if os.environ["GITHUB_SHA"] != git(repo, "rev-parse", "HEAD"):
            raise ValueError("candidate orchestration must start at its trusted workflow commit")
        baseline = git(repo, "rev-parse", "HEAD")
        prefix = policy["candidatePrefix"] + os.environ["GITHUB_RUN_ID"] + "-" + os.environ["GITHUB_RUN_ATTEMPT"] + "/"
        require_generation_baseline(repo, baseline, policy)
        require_unused_candidate_namespace(repo, prefix)
        require_candidate_retention_capacity(repo, policy)
        try:
            canonical = canonical_vendor(repo, baseline)
        except subprocess.CalledProcessError:
            print("Go dependency updates held: canonical baseline vendor reconstruction failed")
            canonical = False
        config = json.loads(Path("renovate.json").read_text())
        if not canonical:
            config["packageRules"].append({"matchManagers": ["gomod"], "enabled": False})
        directory = Path(os.environ["RUNNER_TEMP"]) / "renovate"
        directory.mkdir(exist_ok=True)
        config.update(platform="github", repositories=[policy["repository"]], onboarding=False,
            requireConfig="ignored", branchPrefix=prefix, baseBranchPatterns=[policy["defaultBranch"]],
            allowScripts=False, exposeAllEnv=False, allowedCommands=[], allowedUnsafeExecutions=[],
            hostRules=[{"matchHost": "github.com", "token": os.environ["DEPENDENCY_READ_TOKEN"]},
                {"hostType": "github", "matchHost": "api.github.com", "readOnly": True,
                 "token": os.environ["DEPENDENCY_READ_TOKEN"]}])
        config_path = directory / "config.json"
        generated_inputs = Path(".github/dependency-updates/generated-inputs.sh")
        if generated_inputs.is_file():
            (directory / "generated-inputs.sh").write_bytes(generated_inputs.read_bytes())
            config["allowedCommands"] = ["^bash /config/generated-inputs[.]sh$"]
            config["postUpgradeTasks"] = {"commands": ["bash /config/generated-inputs.sh"],
                "fileFilters": policy["generatedInputs"], "executionMode": "branch"}
        config_path.write_text(json.dumps(config))
        image = policy["renovateImage"]
        try:
            prepare_private_container_config(config_path)
            subprocess.run(["docker", "run", "--rm", "--volume", str(directory) + ":/config:ro",
                "--entrypoint", "renovate-config-validator", image, "--strict", "/config/config.json"], check=True)
            subprocess.run(["docker", "run", "--rm", "--volume", str(directory) + ":/config:ro",
                "--env", "RENOVATE_TOKEN", "--env", "RENOVATE_CONFIG_FILE=/config/config.json",
                "--env", "GOPRIVATE=github.com/axiom-studio/*", image], check=True)
        finally:
            config_path.unlink(missing_ok=True)
            (directory / "generated-inputs.sh").unlink(missing_ok=True)
        branches = [line.split() for line in git(repo, "ls-remote", "--heads", "origin", "refs/heads/" + prefix + "*").splitlines()]
        if len(branches) > 1:
            raise ValueError("bounded generator unexpectedly produced multiple candidates")
        if branches:
            candidate, branch_ref = branches[0]
            git(repo, "fetch", "--no-tags", "origin", branch_ref)
            receipt = inspect_candidate(repo, baseline, candidate, policy, branch_ref[len("refs/heads/"):])
            enforce_vendor_hold(receipt, policy, str(canonical).lower())
        output(baseline=baseline, candidate=branches[0][0] if branches else "",
               branch=branches[0][1][len("refs/heads/"):] if branches else "",
               canonicalVendor=str(canonical).lower())
    elif args.command == "inspect":
        receipt = inspect_candidate(repo, args.baseline, args.candidate, policy, args.branch)
        enforce_vendor_hold(receipt, policy, args.canonical_vendor)
        Path(args.receipt).write_text(json.dumps(receipt, sort_keys=True))
    elif args.command == "clean":
        require_clean_candidate(repo, args.candidate)
    elif args.command == "verify-candidate-vendor":
        verify_candidate_vendor(repo, args.baseline, args.candidate, policy, args.branch, args.canonical_vendor)
    elif args.command == "promote":
        receipt = inspect_candidate(repo, args.baseline, args.candidate, policy, args.branch)
        enforce_vendor_hold(receipt, policy, args.canonical_vendor)
        result = promote_candidate(repo, receipt, policy, verify_workflow_checks)
        Path(args.receipt).write_text(json.dumps(receipt, sort_keys=True))
        with open(os.environ["GITHUB_STEP_SUMMARY"], "a") as summary:
            summary.write(f"Tested dependency commit `{result}` advanced `{policy['defaultBranch']}`.\n\n"
                "This is a source update; no release tag, image publication, or deployment was performed.\n")
            summary.write("\n```json\n" + json.dumps(receipt, sort_keys=True) + "\n```\n")
    elif args.command == "cleanup":
        # This separate step starts only after Actions persists the successful
        # promotion summary. It executes trusted baseline code only.
        try:
            receipt = json.loads(Path(args.receipt).read_text())
            expected = inspect_candidate(repo, receipt["baseline"], receipt["candidate"], policy, receipt["branch"])
            if receipt != expected:
                raise ValueError("cleanup receipt or trusted policy changed")
            verify_workflow_checks(receipt)
            git(repo, "fetch", "--no-tags", "origin", "refs/heads/" + policy["defaultBranch"])
            git(repo, "merge-base", "--is-ancestor", receipt["candidate"], "FETCH_HEAD")
            cleanup_promoted_candidate(repo, receipt, policy)
            cleanup = "Removed this run's unchanged promoted candidate ref with an exact SHA lease."
        except (ValueError, KeyError, OSError, subprocess.CalledProcessError) as error:
            cleanup = "Candidate cleanup held; source promotion remains successful: " + str(error)
        print(cleanup)
        with open(os.environ["GITHUB_STEP_SUMMARY"], "a") as summary:
            summary.write("\n" + cleanup + "\n")


def enforce_vendor_hold(receipt: dict, policy: dict, canonical: str | None) -> None:
    if policy.get("canonicalVendorRequired") and canonical != "true":
        if any(Path(path).name in {"go.mod", "go.sum"} or "vendor" in Path(path).parts
               for path in receipt["changedFiles"]):
            raise ValueError("Go candidate is held until baseline vendor provenance is canonical")


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        raise SystemExit("Dependency update held: " + str(error)) from error
