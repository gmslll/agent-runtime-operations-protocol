from __future__ import annotations

import base64
import csv
import hashlib
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest
import zipfile


PROJECT = Path(__file__).resolve().parents[1]
ROOT = PROJECT.parents[1]
BUILDER = PROJECT / "scripts" / "build_package.py"
VERSION = "0.1.0.dev0"
WHEEL = f"arop_sdk-{VERSION}-py3-none-any.whl"
SDIST = f"arop_sdk-{VERSION}.tar.gz"


def environment() -> dict[str, str]:
    value = {
        key: item
        for key, item in os.environ.items()
        if key.upper()
        not in {
            "PYTHONHOME",
            "PYTHONPATH",
            "PYTHONSTARTUP",
            "PYTHONINSPECT",
            "PYTHONWARNINGS",
            "PYTHONUSERBASE",
            "PIP_CONFIG_FILE",
            "PIP_INDEX_URL",
            "PIP_EXTRA_INDEX_URL",
            "PIP_TRUSTED_HOST",
        }
    }
    value.update(
        {
            "PYTHONDONTWRITEBYTECODE": "1",
            "PYTHONHASHSEED": "0",
            "PIP_CONFIG_FILE": os.devnull,
            "PIP_NO_INDEX": "1",
            "PIP_DISABLE_PIP_VERSION_CHECK": "1",
        }
    )
    return value


def build(directory: Path) -> dict[str, object]:
    output = directory / "dist"
    manifest = directory / "manifest.json"
    subprocess.run(
        [sys.executable, str(BUILDER), "--out-dir", str(output), "--manifest", str(manifest)],
        cwd=ROOT,
        env=environment(),
        check=True,
        capture_output=True,
    )
    return json.loads(manifest.read_text("utf-8"))


class PackageTests(unittest.TestCase):
    def test_two_builds_are_identical_and_inventory_is_exact(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            first = Path(temporary) / "first"
            second = Path(temporary) / "second"
            first.mkdir()
            second.mkdir()
            first_manifest = build(first)
            second_manifest = build(second)
            self.assertEqual(first_manifest, second_manifest)
            self.assertEqual(
                [item["name"] for item in first_manifest["artifacts"]],
                sorted([SDIST, WHEEL]),
            )
            for name in (SDIST, WHEEL):
                self.assertEqual(
                    (first / "dist" / name).read_bytes(),
                    (second / "dist" / name).read_bytes(),
                )
            self.verify_wheel(first / "dist" / WHEEL)
            self.verify_sdist(first / "dist" / SDIST)

    def verify_wheel(self, path: Path) -> None:
        with zipfile.ZipFile(path) as archive:
            names = archive.namelist()
            self.assertEqual(names, sorted(names))
            self.assertTrue(all(name.startswith(("arop/", f"arop_sdk-{VERSION}.dist-info/")) for name in names))
            self.assertFalse(any("tests/" in name or "__pycache__" in name or name.endswith(".pyc") for name in names))
            metadata = archive.read(f"arop_sdk-{VERSION}.dist-info/METADATA")
            self.assertIn(b"Metadata-Version: 2.4\n", metadata)
            self.assertIn(b"Name: arop-sdk\n", metadata)
            self.assertIn(f"Version: {VERSION}\n".encode(), metadata)
            self.assertIn(b"License-Expression: Apache-2.0\n", metadata)
            record_name = f"arop_sdk-{VERSION}.dist-info/RECORD"
            rows = list(csv.reader(io.StringIO(archive.read(record_name).decode())))
            self.assertEqual([row[0] for row in rows], names)
            for name, digest, size in rows:
                if name == record_name:
                    self.assertEqual((digest, size), ("", ""))
                    continue
                data = archive.read(name)
                want = base64.urlsafe_b64encode(hashlib.sha256(data).digest()).rstrip(b"=").decode()
                self.assertEqual((digest, size), ("sha256=" + want, str(len(data))))
            self.assert_no_host_or_secret(archive.read(name) for name in names)

    def verify_sdist(self, path: Path) -> None:
        with tarfile.open(path, "r:gz") as archive:
            members = archive.getmembers()
            names = [member.name for member in members]
            prefix = f"arop_sdk-{VERSION}/"
            self.assertEqual(names, sorted(names))
            self.assertIn(prefix + "pyproject.toml", names)
            self.assertIn(prefix + "scripts/build_package.py", names)
            self.assertIn(prefix + "LICENSE", names)
            self.assertTrue(all(member.isfile() and member.uid == 0 and member.gid == 0 and member.mtime == 0 and member.mode == 0o644 for member in members))
            self.assert_no_host_or_secret(archive.extractfile(member).read() for member in members)

    def assert_no_host_or_secret(self, values) -> None:
        forbidden = (
            str(ROOT).encode(),
            b"opaque-credential",
            b"password=secret",
            b"-----BEGIN PRIVATE KEY-----",
            b"-----BEGIN OPENSSH PRIVATE KEY-----",
        )
        for data in values:
            for sentinel in forbidden:
                self.assertNotIn(sentinel, data)

    def test_clean_venv_installs_offline_and_imports_public_surfaces(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            root.mkdir(exist_ok=True)
            build(root)
            for artifact in (WHEEL, SDIST):
                venv = root / ("venv-" + artifact.removesuffix(".tar.gz").removesuffix(".whl"))
                subprocess.run(
                    [sys.executable, "-m", "venv", str(venv)],
                    env=environment(),
                    check=True,
                )
                python = venv / ("Scripts/python.exe" if os.name == "nt" else "bin/python")
                subprocess.run(
                    [
                        str(python),
                        "-m",
                        "pip",
                        "install",
                        "--no-index",
                        "--no-deps",
                        "--no-build-isolation",
                        str(root / "dist" / artifact),
                    ],
                    env=environment(),
                    check=True,
                    capture_output=True,
                )
                result = subprocess.run(
                    [
                        str(python),
                        "-I",
                        "-c",
                        "import arop,arop.provider,arop.asgi,arop.registry,arop.worker;print(arop.__version__)",
                    ],
                    env=environment(),
                    check=True,
                    capture_output=True,
                    text=True,
                )
                self.assertEqual(result.stdout.strip(), VERSION)


if __name__ == "__main__":
    unittest.main()
