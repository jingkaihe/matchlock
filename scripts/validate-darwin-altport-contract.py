#!/usr/bin/env python3
"""Validate the MATCHLOCK-DARWIN-ALTPORT findings contract artifact.

The contract JSON is published outside the repository at
/home/kaladin/matchlock-work/matchlock-darwin-altport-contract.json. This
validator is the executable specification for that artifact: it checks the
required shape, the exact darwin code paths, the gate results and the
coordinator action on the macOS re-run. `--self-test` exercises every
classifier against a synthetic contract, so the validator cannot silently rot.

Usage:
    python3 scripts/validate-darwin-altport-contract.py [PATH]
    python3 scripts/validate-darwin-altport-contract.py --self-test
"""
from __future__ import annotations

import argparse
import json
import re
import sys

DEFAULT_PATH = "/home/kaladin/matchlock-work/matchlock-darwin-altport-contract.json"
MAC_LOG = "/home/kaladin/matchlock-work/matchlock-acceptance-mac-14f3784.log"
HEX40 = re.compile(r"^[0-9a-f]{40}$")
SHORT_SHA = re.compile(r"^[0-9a-f]{7,40}$")

TOP_STRING_KEYS = (
    "task",
    "bead",
    "run",
    "generatedAt",
    "repo",
    "branch",
    "baselineTree",
    "finalTree",
    "finalTreeFullSha",
)
FINDING_STRING_KEYS = (
    "id",
    "title",
    "status",
    "classification",
    "rootCause",
    "verification",
)
# Exact code paths the contract must name.
ALTPORT_PATH_TOKENS = ("pkg/net/stack_darwin.go", "handlePassthrough")
RESIZE_PATH_TOKENS = ("pkg/vsock", "SendMessage", "pkg/vm/darwin", "ExecInteractive")
COORDINATOR_TESTS = ("TestSDKAltPortTCPPolicyDifferential", "TestCLIRunInteractivePTYResize")


def _as_text(value: object) -> str:
    return json.dumps(value, sort_keys=True)


def _commit_sha(commit: object) -> str | None:
    if isinstance(commit, dict):
        sha = commit.get("sha")
        return sha if isinstance(sha, str) else None
    if isinstance(commit, (list, tuple)) and len(commit) >= 2 and isinstance(commit[0], str):
        return commit[0]
    return None


def _commit_subject(commit: object) -> str | None:
    if isinstance(commit, dict):
        subject = commit.get("subject")
        return subject if isinstance(subject, str) else None
    if isinstance(commit, (list, tuple)) and len(commit) >= 2 and isinstance(commit[1], str):
        return commit[1]
    return None


def validate(contract: object) -> list[str]:
    """Return a list of human-readable problems; empty means valid."""
    errors: list[str] = []
    if not isinstance(contract, dict):
        return ["contract root must be a JSON object"]

    if contract.get("schemaVersion") != 1:
        errors.append("schemaVersion must be 1")

    for key in TOP_STRING_KEYS:
        value = contract.get(key)
        if not isinstance(value, str) or not value.strip():
            errors.append(f"{key} must be a non-empty string")

    if contract.get("task") != "MATCHLOCK-DARWIN-ALTPORT":
        errors.append("task must be MATCHLOCK-DARWIN-ALTPORT")

    full_sha = contract.get("finalTreeFullSha")
    if isinstance(full_sha, str) and not HEX40.match(full_sha):
        errors.append("finalTreeFullSha must be a 40-char lowercase hex sha")

    commits = contract.get("commits")
    commit_shas: list[str] = []
    if not isinstance(commits, list) or not commits:
        errors.append("commits must be a non-empty list")
    else:
        for i, commit in enumerate(commits):
            sha = _commit_sha(commit)
            subject = _commit_subject(commit)
            if sha is None or not SHORT_SHA.match(sha):
                errors.append(f"commits[{i}] must carry a 7-40 char hex sha")
            else:
                commit_shas.append(sha)
            if not subject:
                errors.append(f"commits[{i}] must carry a non-empty subject")
        final_tree = contract.get("finalTree")
        if isinstance(final_tree, str) and final_tree and not any(
            sha == final_tree or sha == full_sha for sha in commit_shas
        ):
            errors.append("commits must include finalTree/finalTreeFullSha")

    findings = contract.get("findings")
    if not isinstance(findings, list) or not findings:
        errors.append("findings must be a non-empty list")
    else:
        seen_ids: set[str] = set()
        for i, finding in enumerate(findings):
            if not isinstance(finding, dict):
                errors.append(f"findings[{i}] must be an object")
                continue
            for key in FINDING_STRING_KEYS:
                value = finding.get(key)
                if not isinstance(value, str) or not value.strip():
                    errors.append(f"findings[{i}].{key} must be a non-empty string")
            fid = finding.get("id")
            if isinstance(fid, str):
                if fid in seen_ids:
                    errors.append(f"duplicate finding id {fid}")
                seen_ids.add(fid)
            if not isinstance(finding.get("exactPath"), str) or not finding.get("exactPath"):
                errors.append(f"findings[{i}].exactPath must be a non-empty string")

        altport = [f for f in findings if all(t in _as_text(f) for t in ALTPORT_PATH_TOKENS)]
        resize = [f for f in findings if all(t in _as_text(f) for t in RESIZE_PATH_TOKENS)]
        if not altport:
            errors.append(
                "no finding names the darwin alt-port path "
                "pkg/net/stack_darwin.go handlePassthrough"
            )
        if not resize:
            errors.append(
                "no finding names the resize desync path "
                "pkg/vsock SendMessage + pkg/vm/darwin ExecInteractive"
            )
        if altport and "product" not in altport[0].get("classification", ""):
            errors.append("alt-port finding classification must be product")
        if resize and "test" not in resize[0].get("classification", ""):
            errors.append("resize finding classification must include test")

    evidence = contract.get("evidencePaths")
    if not isinstance(evidence, list) or not evidence:
        errors.append("evidencePaths must be a non-empty list")
    else:
        if not all(isinstance(p, str) and p for p in evidence):
            errors.append("evidencePaths entries must be non-empty strings")
        if MAC_LOG not in evidence:
            errors.append(f"evidencePaths must include the mac baseline log {MAC_LOG}")

    coordinator = contract.get("coordinatorAction")
    if coordinator is None:
        errors.append("coordinatorAction is required")
    else:
        text = _as_text(coordinator)
        for test in COORDINATOR_TESTS:
            if test not in text:
                errors.append(f"coordinatorAction must compare {test}")
        if "mac" not in text.lower():
            errors.append("coordinatorAction must re-run the macOS acceptance suite")

    gates = contract.get("qualityGates")
    if not isinstance(gates, dict):
        errors.append("qualityGates must be an object")
    else:
        for key in ("crossCompile", "linux"):
            if not isinstance(gates.get(key), dict):
                errors.append(f"qualityGates.{key} must be an object")

    if not isinstance(contract.get("vmCleanup"), dict):
        errors.append("vmCleanup must be an object")

    return errors


def _self_test() -> int:
    valid = {
        "schemaVersion": 1,
        "task": "MATCHLOCK-DARWIN-ALTPORT",
        "bead": "tamandua-6sy.33.10.35",
        "run": "run-7fc14e14-f93f-42d2-bbbc-d3c4be87d8a6",
        "generatedAt": "2026-09-15T00:00:00Z",
        "repo": "/opt/matchlock-fix.T8kR2wYq",
        "branch": "fix/matchlock-darwin-altport-resize",
        "baselineTree": "14f3784",
        "finalTree": "abcdef1",
        "finalTreeFullSha": "a" * 40,
        "commits": [["abcdef1", "feat: US-006 - contract"], ["abcdef0", "feat: US-001"]],
        "findings": [
            {
                "id": "US-FINDING-1",
                "title": "darwin alt-port",
                "status": "fixed",
                "classification": "product",
                "exactPath": "pkg/net/stack_darwin.go NetworkStack.handlePassthrough -> copyWithCancel",
                "rootCause": "one context cancels the pending copy",
                "verification": "shared relay test",
            },
            {
                "id": "US-FINDING-2",
                "title": "resize flake",
                "status": "fixed",
                "classification": "product+test",
                "exactPath": "pkg/vsock SendMessage plus pkg/vm/darwin ExecInteractive stdin/resize",
                "rootCause": "two writes interleave",
                "verification": "FrameWriter single locked write",
            },
        ],
        "evidencePaths": [MAC_LOG, "/home/kaladin/matchlock-work/darwin-altport-us005-crosscompile.log"],
        "coordinatorAction": {
            "action": "re-run the darwin/macOS acceptance suite on the Mac",
            "compare": ["TestSDKAltPortTCPPolicyDifferential", "TestCLIRunInteractivePTYResize"],
        },
        "qualityGates": {"crossCompile": {"exitCode": 0}, "linux": {"exitCode": 0}},
        "vmCleanup": {"finalList": "empty"},
    }
    if validate(valid):
        print("self-test: valid fixture was rejected", file=sys.stderr)
        print("\n".join(validate(valid)), file=sys.stderr)
        return 1

    mutations: list[tuple[str, dict]] = []
    mutations.append(("bad schemaVersion", {**valid, "schemaVersion": 2}))
    mutations.append(("missing finalTreeFullSha", {k: v for k, v in valid.items() if k != "finalTreeFullSha"}))
    mutations.append(("short finalTreeFullSha", {**valid, "finalTreeFullSha": "abc"}))
    mutations.append(("empty commits", {**valid, "commits": []}))
    mutations.append(("finalTree not in commits", {**valid, "finalTree": "9999999"}))
    mutations.append(("empty findings", {**valid, "findings": []}))
    mutations.append(("missing alt-port path", {**valid, "findings": [valid["findings"][1]]}))
    mutations.append(("missing resize path", {**valid, "findings": [valid["findings"][0]]}))
    mutations.append(("missing mac log", {**valid, "evidencePaths": ["/tmp/other.log"]}))
    mutations.append(("coordinator missing test", {**valid, "coordinatorAction": {"action": "re-run on mac"}}))
    mutations.append(("missing qualityGates", {k: v for k, v in valid.items() if k != "qualityGates"}))
    mutations.append(("missing vmCleanup", {k: v for k, v in valid.items() if k != "vmCleanup"}))

    for name, mutated in mutations:
        if not validate(mutated):
            print(f"self-test: mutation not detected: {name}", file=sys.stderr)
            return 1

    print("self-test: OK")
    return 0


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("path", nargs="?", default=DEFAULT_PATH)
    parser.add_argument("--self-test", action="store_true")
    args = parser.parse_args(argv)

    if args.self_test:
        return _self_test()

    try:
        with open(args.path, encoding="utf-8") as fh:
            contract = json.load(fh)
    except FileNotFoundError:
        print(f"contract not found: {args.path}", file=sys.stderr)
        return 1
    except json.JSONDecodeError as exc:
        print(f"contract is not valid JSON: {exc}", file=sys.stderr)
        return 1

    errors = validate(contract)
    if errors:
        for err in errors:
            print(f"INVALID: {err}", file=sys.stderr)
        return 1

    commits = len(contract["commits"])
    findings = len(contract["findings"])
    print(f"CONTRACT VALID: {args.path} ({commits} commits, {findings} findings)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
