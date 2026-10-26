#!/usr/bin/env python3
"""추적 소스와 새 파일의 잔여 Han 문자를 검사한다.

지원 로캘·원본 문서는 파일 단위, 그 밖의 호환성/증거 예외는 정확한 줄의
SHA-256과 발생 횟수로 승인한다. 새 중국어 문구를 기존 파일에 추가해도
검사를 통과하지 못한다. 원문 내용이나 자격증명은 출력하지 않는다.
"""
import argparse
from collections import Counter
import hashlib
import json
from pathlib import Path
import re
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[1]
MANIFEST = ROOT / "docs/localization/retained-han.json"
# rg의 Unicode Script=Han 범위와 대조한 정적 범위. 부수·반복 기호·확장 I도 포함한다.
HAN = re.compile(r"[\u2e80-\u2e99\u2e9b-\u2ef3\u2f00-\u2fd5\u3005\u3007\u3021-\u3029\u3038-\u303b\u3400-\u4dbf\u4e00-\u9fff\uf900-\ufa6d\ufa70-\ufad9\U00016fe2-\U00016fe3\U00016ff0-\U00016ff1\U00020000-\U0002a6df\U0002a700-\U0002b739\U0002b740-\U0002b81d\U0002b820-\U0002cea1\U0002ceb0-\U0002ebe0\U0002ebf0-\U0002ee5d\U0002f800-\U0002fa1d\U00030000-\U0003134a\U00031350-\U000323af]")


def inventory():
    paths = subprocess.check_output(
        ["git", "ls-files", "--cached", "--others", "--exclude-standard", "-z"],
        cwd=ROOT,
    ).decode().split("\0")
    result = {}
    for name in sorted(set(paths) - {""}):
        path = ROOT / name
        if path.is_symlink() or not path.is_file():
            continue
        try:
            content = path.read_text(encoding="utf-8")
        except UnicodeError:
            continue
        if "\0" in content:
            continue
        rows = [
            {"line": number, "sha256": hashlib.sha256(line.encode()).hexdigest()}
            for number, line in enumerate(content.splitlines(), 1)
            if HAN.search(line)
        ]
        if rows:
            result[name] = rows
    return result


def check(current, manifest):
    failures = []
    approved = manifest["files"]
    for name in sorted(set(current) | set(approved)):
        entry = approved.get(name)
        rows = current.get(name, [])
        if entry is None:
            failures.append({"file": name, "reason": "unclassified", "lines": [r["line"] for r in rows]})
        elif not entry.get("reason"):
            failures.append({"file": name, "reason": "missing rationale"})
        elif entry["mode"] == "localized-resource":
            if not rows:
                failures.append({"file": name, "reason": "stale resource exception"})
        elif entry["mode"] == "exact-lines":
            actual = Counter(r["sha256"] for r in rows)
            expected = Counter(entry["sha256_counts"])
            if actual != expected:
                failures.append({"file": name, "reason": "retained lines changed", "lines": [r["line"] for r in rows if r["sha256"] not in expected], "added": sum((actual - expected).values()), "removed": sum((expected - actual).values())})
        else:
            failures.append({"file": name, "reason": "unknown exception mode"})
    return failures


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true", help="승인된 잔여 목록과 대조하며 새 문구 또는 오래된 예외가 있으면 실패")
    args = parser.parse_args()
    current = inventory()
    result = {"files": len(current), "han_lines": sum(map(len, current.values()))}
    if args.check:
        manifest = json.loads(MANIFEST.read_text())
        failures = check(current, manifest)
        result.update(status="fail" if failures else "pass", failures=failures)
    else:
        failures = []
        result["inventory"] = current
    print(json.dumps(result, ensure_ascii=False, indent=2))
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
