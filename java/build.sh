#!/usr/bin/env bash
# Build the Java side of the relay with nothing but a JDK.
#
# There is no Maven or Gradle step on purpose: the dependencies are vendored in
# lib/ and pinned by lib/deps.sha256, so this works offline and on a machine
# that has never seen a build tool. Run tools/resolve_deps.py if you need to
# re-fetch or bump them.
#
#   ./build.sh          compile main + tools into out/ and out-tools/
#   ./build.sh clean    remove both output trees
#   ./build.sh check    compile, then run the three probes
#
# The probes read the block palettes from ../internal/blocktranslate/data, so
# this tree expects to sit inside the relay repository.

set -euo pipefail

cd "$(dirname "$0")"

LIB="lib/*"
MAIN_OUT="out"
TOOL_OUT="out-tools"
MAIN_SRC="src/main/java"
TOOL_SRC="src/tools/java"
DATA_DIR="../internal/blocktranslate/data"

# javac is a Windows binary under Git Bash / MSYS2, and it wants ';' where the
# shell wants ':'. Get this wrong and every cross-tree reference fails to
# resolve with "cannot find symbol" on classes that are plainly there.
case "$(uname -s)" in
    MINGW*|MSYS*|CYGWIN*) SEP=';' ;;
    *)                    SEP=':' ;;
esac

javac_bin="${JAVAC:-javac}"
java_bin="${JAVA:-java}"

clean() {
    for d in "$MAIN_OUT" "$TOOL_OUT"; do
        [ -d "$d" ] && find "$d" -depth -delete
    done
}

build() {
    if [ ! -d lib ] || [ -z "$(ls -A lib/*.jar 2>/dev/null)" ]; then
        echo "lib/ has no jars; run: python3 tools/resolve_deps.py" >&2
        exit 1
    fi

    mkdir -p "$MAIN_OUT" "$TOOL_OUT"

    echo "==> compiling main"
    "$javac_bin" -encoding UTF-8 -Xlint:all -cp "$LIB" -d "$MAIN_OUT" \
        $(find "$MAIN_SRC" -name '*.java')

    echo "==> compiling tools"
    "$javac_bin" -encoding UTF-8 -Xlint:all -cp "$LIB$SEP$MAIN_OUT" -d "$TOOL_OUT" \
        $(find "$TOOL_SRC" -name '*.java')

    echo "==> done"
    echo "    main classes : $MAIN_OUT"
    echo "    tool classes : $TOOL_OUT"
    echo "    run a probe  : $java_bin -cp "$LIB$SEP$MAIN_OUT$SEP$TOOL_OUT" xvrelay.tools.PaletteProbe"
}

check() {
    echo "==> CodecProbe"
    "$java_bin" -cp "$LIB$SEP$MAIN_OUT$SEP$TOOL_OUT" xvrelay.tools.CodecProbe

    echo "==> PaletteProbe"
    "$java_bin" -cp "$LIB$SEP$MAIN_OUT$SEP$TOOL_OUT" xvrelay.tools.PaletteProbe "$DATA_DIR"

    echo "==> MappingProbe"
    "$java_bin" -cp "$LIB$SEP$MAIN_OUT$SEP$TOOL_OUT" xvrelay.tools.MappingProbe "$DATA_DIR"
}

case "${1:-build}" in
    build)  build ;;
    clean)  clean ;;
    check)  build; check ;;
    *)      echo "usage: $0 [build|clean|check]" >&2; exit 2 ;;
esac
