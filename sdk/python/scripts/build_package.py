#!/usr/bin/env python3
"""Build deterministic wheel/sdist artifacts without network or credentials."""

from __future__ import annotations

import argparse
import base64
import csv
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import stat
import tarfile
import tomllib
import zipfile


def main() -> None:
    parser = argparse.ArgumentParser(allow_abbrev=False)
    parser.add_argument("--out-dir", required=True)
    parser.add_argument("--manifest", required=True)
    arguments = parser.parse_args()
    project = Path(__file__).resolve().parents[1]
    configuration = tomllib.loads((project / "pyproject.toml").read_text("utf-8"))
    metadata = configuration["project"]
    policy = configuration["tool"]["arop-package"]
    name = exact_text(metadata, "name")
    version = exact_text(metadata, "version")
    if name != "arop-sdk" or version != "0.1.0.dev0":
        raise SystemExit("unexpected package identity")
    if metadata.get("license") != "Apache-2.0" or metadata.get("requires-python") != ">=3.11":
        raise SystemExit("package policy mismatch")
    source = project / exact_text(policy, "source-root") / exact_text(policy, "package-root")
    files = collect(source, project / "src")
    output = Path(arguments.out_dir)
    output.mkdir(mode=0o700, parents=True, exist_ok=True)
    wheel_name = f"arop_sdk-{version}-py3-none-any.whl"
    sdist_name = f"arop_sdk-{version}.tar.gz"
    _write_wheel(
        output / wheel_name,
        files,
        metadata,
        version,
        license_bytes(project),
    )
    _write_sdist(output / sdist_name, project, files, version)
    artifacts = []
    for artifact in (output / sdist_name, output / wheel_name):
        data = artifact.read_bytes()
        artifacts.append(
            {"name": artifact.name, "sha256": hashlib.sha256(data).hexdigest(), "bytes": len(data)}
        )
    manifest = {
        "schema_version": 1,
        "name": name,
        "version": version,
        "artifacts": sorted(artifacts, key=lambda item: item["name"]),
    }
    destination = Path(arguments.manifest)
    destination.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    destination.write_bytes(json.dumps(manifest, sort_keys=True, separators=(",", ":")).encode() + b"\n")


def collect(package: Path, source_root: Path) -> list[tuple[str, bytes]]:
    values: list[tuple[str, bytes]] = []
    for path in sorted(package.rglob("*")):
        if "__pycache__" in path.parts:
            continue
        info = path.lstat()
        if stat.S_ISLNK(info.st_mode):
            raise SystemExit("package source contains symlink")
        if path.is_dir():
            continue
        if not stat.S_ISREG(info.st_mode) or path.suffix not in {".py", ".typed"}:
            raise SystemExit("package source contains unsupported file")
        relative = path.relative_to(source_root).as_posix()
        data = path.read_bytes()
        if (b"-----BEGIN " + b"PRIVATE KEY-----") in data or (
            b"-----BEGIN " + b"OPENSSH PRIVATE KEY-----"
        ) in data:
            raise SystemExit("package source contains secret sentinel")
        values.append((relative, data))
    if not values or values[0][0] != "arop/__init__.py":
        raise SystemExit("package source inventory is incomplete")
    return values


def _write_wheel(
    destination: Path,
    files: list[tuple[str, bytes]],
    metadata: dict[str, object],
    version: str,
    license_text: bytes,
) -> None:
    dist = f"arop_sdk-{version}.dist-info"
    extra = {
        f"{dist}/METADATA": metadata_bytes(metadata),
        f"{dist}/WHEEL": b"Wheel-Version: 1.0\nGenerator: arop-python-package-v1\nRoot-Is-Purelib: true\nTag: py3-none-any\n",
        f"{dist}/licenses/LICENSE": license_text,
    }
    entries = sorted(files + list(extra.items()), key=lambda item: item[0])
    record_name = f"{dist}/RECORD"
    by_name = dict(entries)
    record_buffer = io.StringIO(newline="")
    writer = csv.writer(record_buffer, lineterminator="\n")
    records = []
    for name in sorted([*by_name, record_name]):
        if name == record_name:
            records.append((name, "", ""))
        else:
            data = by_name[name]
            records.append((name, wheel_digest(data), str(len(data))))
    writer.writerows(records)
    entries.append((record_name, record_buffer.getvalue().encode()))
    with zipfile.ZipFile(destination, "w", zipfile.ZIP_DEFLATED, compresslevel=9) as archive:
        for name, data in sorted(entries, key=lambda item: item[0]):
            info = zipfile.ZipInfo(name, (1980, 1, 1, 0, 0, 0))
            info.compress_type = zipfile.ZIP_DEFLATED
            info.external_attr = (stat.S_IFREG | 0o644) << 16
            info.create_system = 3
            archive.writestr(info, data, compress_type=zipfile.ZIP_DEFLATED, compresslevel=9)


def _write_sdist(
    destination: Path,
    project: Path,
    files: list[tuple[str, bytes]],
    version: str,
) -> None:
    prefix = f"arop_sdk-{version}"
    entries = [("pyproject.toml", (project / "pyproject.toml").read_bytes())]
    entries.append(("LICENSE", license_bytes(project)))
    entries.append(("scripts/build_package.py", (project / "scripts" / "build_package.py").read_bytes()))
    entries.extend(("src/" + name, data) for name, data in files)
    with destination.open("wb") as raw:
        with gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0, compresslevel=9) as compressed:
            with tarfile.open(fileobj=compressed, mode="w", format=tarfile.GNU_FORMAT) as archive:
                for name, data in sorted(entries, key=lambda item: item[0]):
                    info = tarfile.TarInfo(f"{prefix}/{name}")
                    info.size = len(data)
                    info.mtime = 0
                    info.mode = 0o644
                    info.uid = info.gid = 0
                    info.uname = info.gname = ""
                    archive.addfile(info, io.BytesIO(data))


def metadata_bytes(metadata: dict[str, object]) -> bytes:
    return (
        "Metadata-Version: 2.4\n"
        f"Name: {exact_text(metadata, 'name')}\n"
        f"Version: {exact_text(metadata, 'version')}\n"
        f"Summary: {exact_text(metadata, 'description')}\n"
        f"Requires-Python: {exact_text(metadata, 'requires-python')}\n"
        "License-Expression: Apache-2.0\n"
        "License-File: LICENSE\n"
    ).encode()


def wheel_digest(data: bytes) -> str:
    value = base64.urlsafe_b64encode(hashlib.sha256(data).digest()).rstrip(b"=").decode()
    return "sha256=" + value


def exact_text(value: dict[str, object], key: str) -> str:
    item = value.get(key)
    if not isinstance(item, str) or not item:
        raise SystemExit(f"missing {key}")
    return item


def license_bytes(project: Path) -> bytes:
    """Read LICENSE from either the repository layout or an unpacked sdist."""
    candidates = (project / "LICENSE", project.parents[1] / "LICENSE")
    for candidate in candidates:
        try:
            information = candidate.lstat()
        except FileNotFoundError:
            continue
        if stat.S_ISLNK(information.st_mode) or not stat.S_ISREG(information.st_mode):
            raise SystemExit("package license is not a regular file")
        return candidate.read_bytes()
    raise SystemExit("package license is missing")


def build_wheel(
    wheel_directory: str,
    config_settings: dict[str, object] | None = None,
    metadata_directory: str | None = None,
) -> str:
    del config_settings, metadata_directory
    project = Path.cwd()
    configuration = tomllib.loads((project / "pyproject.toml").read_text("utf-8"))
    metadata = configuration["project"]
    version = exact_text(metadata, "version")
    name = f"arop_sdk-{version}-py3-none-any.whl"
    files = collect(project / "src" / "arop", project / "src")
    destination = Path(wheel_directory)
    destination.mkdir(mode=0o700, parents=True, exist_ok=True)
    _write_wheel(
        destination / name,
        files,
        metadata,
        version,
        license_bytes(project),
    )
    return name


def build_sdist(
    sdist_directory: str, config_settings: dict[str, object] | None = None
) -> str:
    del config_settings
    project = Path.cwd()
    configuration = tomllib.loads((project / "pyproject.toml").read_text("utf-8"))
    version = exact_text(configuration["project"], "version")
    name = f"arop_sdk-{version}.tar.gz"
    files = collect(project / "src" / "arop", project / "src")
    destination = Path(sdist_directory)
    destination.mkdir(mode=0o700, parents=True, exist_ok=True)
    _write_sdist(destination / name, project, files, version)
    return name


if __name__ == "__main__":
    main()
