#!/usr/bin/env python3
"""Resolve the Java relay's Maven dependencies without Maven.

Why this exists: the relay is built with plain `javac` (see java/build.sh) so
that contributors do not need Maven or Gradle installed. That leaves the job of
walking the dependency graph to this script.

What it does
------------
1. Breadth-first walk of the POM dependency graph starting from ROOTS.
2. Honours <dependencyManagement> BOM imports and property placeholders
   (${project.version}, ${revision}, ...).
3. Resolves -SNAPSHOT versions to a concrete timestamped build via
   maven-metadata.xml, so the result is reproducible.
4. Downloads each jar into java/lib/ and writes java/lib/deps.sha256.

Run it only when you want to refresh the vendored jars:

    python3 java/tools/resolve_deps.py            # verify existing jars
    python3 java/tools/resolve_deps.py --update   # re-download everything

The normal build (java/build.sh) never calls this; it just uses java/lib/*.jar.
"""

from __future__ import annotations

import hashlib
import os
import re
import sys
import urllib.request
import xml.etree.ElementTree as ET

# Repositories, tried in order. CloudburstMC publishes only to OpenCollab;
# everything else lives on Maven Central.
REPOS = [
    "https://repo.opencollab.dev/main",
    "https://repo.maven.apache.org/maven2",
]

NS = "{http://maven.apache.org/POM/4.0.0}"

ROOTS = [
    # Bedrock protocol codecs. Version 3.x ships the whole version ladder
    # (v1001 / v2168 / v2169 / v2193 / v2225) inside a single artifact, which is
    # what makes cross-version translation possible from Java at all.
    ("org.cloudburstmc.protocol", "bedrock-codec", "3.0.0.Beta15-SNAPSHOT"),
    ("org.cloudburstmc.protocol", "bedrock-connection", "3.0.0.Beta15-SNAPSHOT"),
    # RakNet transport (Netty). Used for the client-facing listener and for
    # dialling plain LAN worlds.
    ("org.cloudburstmc.netty", "netty-transport-raknet", "4.1.51.Final"),
    # NBT reader/writer: block palettes and LevelChunk block-entity payloads.
    ("org.cloudburstmc", "nbt", "3.1.0.Final"),
    # CloudburstMC logs through slf4j and throws NoClassDefFoundError without it
    # (measured: every codec failed to initialise). slf4j-simple is the binding so
    # the relay keeps zero logging configuration.
    ("org.slf4j", "slf4j-api", "2.0.16"),
    ("org.slf4j", "slf4j-simple", "2.0.16"),
]

# Nothing in the runtime needs these; skipping them keeps java/lib/ small.
SKIP_GROUPS = {
    "org.junit.jupiter",
    "org.junit.platform",
    "junit",
    "org.mockito",
    "com.google.code.gson",
}
SKIP_ARTIFACTS = {"logback-classic", "logback-core"}

SKIP_SCOPES = {"test", "provided"}

LIB = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "lib")


USER_AGENT = "xvrelay-deps/1.0 (+https://github.com/Datkiri/bedrock-cross-version-relay)"


def fetch(url: str, timeout: int = 60) -> bytes | None:
    # repo.opencollab.dev answers 403 to urllib's default User-Agent, so always
    # send one. (Measured: default UA -> 403, custom UA -> 200.)
    req = urllib.request.Request(url, headers={"User-Agent": USER_AGENT})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.read()
    except Exception:
        return None


def tag(node: ET.Element) -> str:
    """Element tag with any XML namespace stripped.

    Maven POMs are namespaced, but the maven-metadata.xml files served by
    repo.opencollab.dev are not — matching on plain local names works for both.
    """
    return node.tag.rsplit("}", 1)[-1]


def children(node: ET.Element | None, name: str) -> list[ET.Element]:
    if node is None:
        return []
    return [c for c in node if tag(c) == name]


def child(node: ET.Element | None, name: str) -> ET.Element | None:
    if node is None:
        return None
    for c in node:
        if tag(c) == name:
            return c
    return None


def text(node: ET.Element | None, name: str) -> str | None:
    found = child(node, name)
    return found.text.strip() if found is not None and found.text else None


def find_artifact(group: str, artifact: str, version: str) -> tuple[str, str] | tuple[None, None]:
    """Return (base_url, concrete_version) for an artifact, or (None, None)."""
    path = f"{group.replace('.', '/')}/{artifact}/{version}"
    for repo in REPOS:
        if version.endswith("-SNAPSHOT"):
            meta = fetch(f"{repo}/{path}/maven-metadata.xml")
            if not meta:
                continue
            try:
                root = ET.fromstring(meta)
            except ET.ParseError:
                continue
            stamp = None
            build = None
            for node in root.iter():
                if tag(node) == "timestamp":
                    stamp = (node.text or "").strip() or None
                elif tag(node) == "buildNumber":
                    build = (node.text or "").strip() or None
            if not stamp:
                continue
            base = version[: -len("-SNAPSHOT")]
            return f"{repo}/{path}", f"{base}-{stamp}-{build}"
        else:
            pom = fetch(f"{repo}/{path}/{artifact}-{version}.pom")
            if pom is not None:
                return f"{repo}/{path}", version
    return None, None


def parent_props(root: ET.Element, depth: int = 0) -> dict[str, str]:
    """Properties inherited from the <parent> POM chain.

    Needed because some artifacts reference ${netty.version} without defining it
    themselves — the property lives in the parent POM. Without this the version
    stays unresolved and the artifact is silently skipped.
    """
    if depth > 4:
        return {}
    parent = child(root, "parent")
    if parent is None:
        return {}
    g = text(parent, "groupId")
    a = text(parent, "artifactId")
    v = text(parent, "version")
    if not (g and a and v):
        return {}
    base, concrete = find_artifact(g, a, v)
    if not base:
        return {}
    raw = fetch(f"{base}/{a}-{concrete}.pom")
    if not raw:
        return {}
    try:
        proot = ET.fromstring(raw)
    except ET.ParseError:
        return {}
    merged = parent_props(proot, depth + 1)
    merged.update(pom_props(proot))
    return merged


def pom_props(root: ET.Element) -> dict[str, str]:
    props: dict[str, str] = {}
    for section in (child(root, "properties"), child(root, "parent")):
        if section is None:
            continue
        for item in section:
            if item.text:
                props[tag(item)] = item.text.strip()
    for name in ("groupId", "artifactId", "version"):
        value = text(root, name)
        if value:
            props[f"project.{name}"] = value
    return props


def interpolate(value: str | None, props: dict[str, str], depth: int = 0) -> str | None:
    if value is None or depth > 8:
        return value
    out = value
    for match in re.finditer(r"\$\{([^}]+)\}", value):
        key = match.group(1)
        if key in props:
            out = out.replace(match.group(0), props[key])
    if "${" in out and out != value:
        return interpolate(out, props, depth + 1)
    return out


def deps_of(pom_bytes: bytes) -> tuple[list[tuple[str, str, str]], dict[tuple[str, str], str]]:
    """Return (deps, managed_versions) for one POM."""
    root = ET.fromstring(pom_bytes)
    props = parent_props(root)
    props.update(pom_props(root))

    managed: dict[tuple[str, str], str] = {}
    dm = child(root, "dependencyManagement")
    container = child(dm, "dependencies")
    for dep in children(container, "dependency"):
        g = interpolate(text(dep, "groupId"), props)
        a = interpolate(text(dep, "artifactId"), props)
        v = interpolate(text(dep, "version"), props)
        if not (g and a and v):
            continue
        if text(dep, "scope") == "import":
            # BOM: pull in its managed versions.
            for bg, ba, bv in read_bom(g, a, v):
                managed[(bg, ba)] = bv
        else:
            managed[(g, a)] = v

    deps: list[tuple[str, str, str]] = []
    for dep in children(child(root, "dependencies"), "dependency"):
        scope = text(dep, "scope") or "compile"
        if scope in SKIP_SCOPES or text(dep, "optional") == "true":
            continue
        g = interpolate(text(dep, "groupId"), props)
        a = interpolate(text(dep, "artifactId"), props)
        v = interpolate(text(dep, "version"), props)
        if not (g and a):
            continue
        if not v:
            v = managed.get((g, a))
        if not v:
            continue
        deps.append((g, a, v))
    return deps, managed


def read_bom(group: str, artifact: str, version: str) -> list[tuple[str, str, str]]:
    base, concrete = find_artifact(group, artifact, version)
    if not base:
        return []
    raw = fetch(f"{base}/{artifact}-{concrete}.pom")
    if not raw:
        return []
    root = ET.fromstring(raw)
    props = pom_props(root)
    out: list[tuple[str, str, str]] = []
    container = child(child(root, "dependencyManagement"), "dependencies")
    for dep in children(container, "dependency"):
        g = interpolate(text(dep, "groupId"), props)
        a = interpolate(text(dep, "artifactId"), props)
        v = interpolate(text(dep, "version"), props)
        if g and a and v:
            out.append((g, a, v))
    return out


def main() -> int:
    update = "--update" in sys.argv
    os.makedirs(LIB, exist_ok=True)

    queue = list(ROOTS)
    seen: set[tuple[str, str]] = set()
    resolved: list[tuple[str, str, str, str, str]] = []  # group, artifact, ver, base, concrete
    problems: list[str] = []

    while queue:
        group, artifact, version = queue.pop(0)
        key = (group, artifact)
        if key in seen or group in SKIP_GROUPS or artifact in SKIP_ARTIFACTS:
            continue
        seen.add(key)

        base, concrete = find_artifact(group, artifact, version)
        if not base:
            problems.append(f"{group}:{artifact}:{version} — not found in any repo")
            continue
        resolved.append((group, artifact, version, base, concrete))

        pom = fetch(f"{base}/{artifact}-{concrete}.pom")
        if not pom:
            continue
        try:
            deps, _ = deps_of(pom)
        except ET.ParseError as exc:
            problems.append(f"{group}:{artifact}:{version} — bad POM: {exc}")
            continue
        for dep in deps:
            if (dep[0], dep[1]) not in seen:
                queue.append(dep)

    lines: list[str] = []
    total = 0
    for group, artifact, version, base, concrete in sorted(resolved):
        name = f"{artifact}-{concrete}.jar"
        dest = os.path.join(LIB, name)
        url = f"{base}/{artifact}-{concrete}.jar"

        if update or not os.path.exists(dest):
            blob = fetch(url, timeout=180)
            if blob is None:
                problems.append(f"{group}:{artifact}:{version} — jar download failed")
                continue
            with open(dest, "wb") as fh:
                fh.write(blob)
            print(f"downloaded {name} ({len(blob)} bytes)")
        with open(dest, "rb") as fh:
            digest = hashlib.sha256(fh.read()).hexdigest()
        size = os.path.getsize(dest)
        total += size
        lines.append(f"{digest}  {name}  # {group}:{artifact}:{version}")

    manifest = os.path.join(LIB, "deps.sha256")
    with open(manifest, "w", encoding="utf-8", newline="\n") as fh:
        fh.write("\n".join(lines) + "\n")

    print(f"\n{len(lines)} jars, {total / 1048576:.1f} MB -> {LIB}")
    print(f"manifest -> {manifest}")
    if problems:
        print("\nPROBLEMS:")
        for p in problems:
            print("  " + p)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
