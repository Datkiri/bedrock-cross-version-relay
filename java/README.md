# Java port — status

This directory holds a JVM reimplementation of the relay. **It is not a working
relay yet.** What exists is the part that was worth getting exactly right before
anything else: block state translation, plus the machinery to load the Bedrock
codecs and prove the version ladder works.

Read the table before you decide whether this is useful to you.

| Layer | Go (`cmd/xvrelay`) | Java (`java/`) |
|---|---|---|
| Block state hash | done, tested | **done, byte-identical, measured** |
| Block palette loading | done, pinned | **done, re-verified at load time** |
| Block state mapping | done, measured | **done, measured** |
| Bedrock codecs (1001/2168/2169/2193) | via gophertunnel fork | **load and encode/decode** |
| LAN discovery (UDP 7551) | done | not written |
| RakNet listener / dialer | via gophertunnel | not written |
| NetherNet transport (WebRTC) | via go-nethernet fork | not written |
| Spawn gate, chunk burst capture | done | not written |
| Chunk / SubChunk translation | done | not written |
| Live session | verified on one path | **never run** |

So: the Java tree compiles, its three probes pass, and its output agrees with
the Go side on every state in all three palettes. It cannot relay a client.
Nothing here has ever been pointed at a real Minecraft client.

## Build

No Maven, no Gradle. A JDK 21 and the jars in `lib/` are the whole toolchain.

```bash
./build.sh          # compile main + tools
./build.sh check    # compile, then run CodecProbe, PaletteProbe, MappingProbe
./build.sh clean
```

or from the repository root: `make java`, `make java-check`, `make java-clean`.

## Dependencies

`lib/` contains 28 jars, 7.8 MB, pinned by SHA-256 in `lib/deps.sha256`. They
are vendored rather than fetched at build time for the same reason the Go side
vendors `third_party/`: the build should work offline and should not silently
drift.

| artifact | version | why |
|---|---|---|
| `bedrock-codec` | 3.0.0.Beta15 | the whole version ladder in one jar: `v1001`, `v2168`, `v2169`, `v2193`, `v2225` |
| `bedrock-connection` | 3.0.0.Beta15 | RakNet session glue around the codec |
| `netty-transport-raknet` | 4.1.51.Final | RakNet transport |
| `nbt` | 3.1.0.Final | reading the pinned palette files |
| `slf4j-api` + `slf4j-simple` | 2.0.16 | not optional — see below |

Two traps cost real time here and are worth writing down:

1. **CloudburstMC is not on Maven Central.** It lives at
   `https://repo.opencollab.dev/main` only. A Central-only resolver finds
   `nbt` and nothing else.
2. **`repo.opencollab.dev` answers 403 to urllib's default User-Agent.** Any
   resolver needs an explicit UA string, or every CloudburstMC fetch fails.
3. **`bedrock-codec` needs slf4j at class-init time.** Without it, every codec
   throws `NoClassDefFoundError: org/slf4j/LoggerFactory` before it reads a
   single byte. It is a hard runtime dependency of the static initialiser, not
   a logging nicety.

`tools/resolve_deps.py` handles all three and rewrites `lib/deps.sha256`:

```bash
python3 tools/resolve_deps.py            # resolve, download, write checksums
python3 tools/resolve_deps.py --verify   # re-check existing jars only
```

It resolves `-SNAPSHOT` to a concrete timestamped build through
`maven-metadata.xml` (namespace-agnostically — that file is served without an
XML namespace), and walks the `<parent>` POM chain so `${netty.version}` and
friends resolve to real versions.

## Block translation

`StateHash` reproduces `internal/blocktranslate/hash.go` exactly: FNV-1a over
the NBT document, unnamed root compound, `name` string tag, `states` compound
with one typed tag per key in sorted order. Getting this wrong silently breaks
every mapping, so `Palette` does not trust the pinned file — it re-derives all
56 503 hashes at load time and throws `IllegalStateException` if any of them
disagrees with the pinned `network_id`.

`BlockMapper` maps in two passes, exactly like the Go version:

1. **Exact** — same name, same full property set, matching hash.
2. **Reduced** — same name, source properties are a subset of the target's,
   with the missing properties at their default values.

When several targets survive pass 2 the mapper picks the **neutral variant**,
the candidate whose extra properties are all zero. This case is not rare:
1.26.50 added `minecraft:connection_{north,east,south,west}` to panes and
stairs, so one 1.26.30 pane state has 56 candidates. Before this rule those
states fell through to the placeholder; now they render as unconnected panes.

Anything still ambiguous becomes `minecraft:info_update` — visible, counted,
never air, and never a guess.

## Measured

`./build.sh check`, JDK 21.0.12.1, all four pairs:

```
CodecProbe
proto 1001  ok    protocol=1001  minecraftVersion=1.26.30  raknet=11  packets=187
proto 2168  ok    protocol=2168  minecraftVersion=1.26.40  raknet=11  packets=187
proto 2169  ok    protocol=2169  minecraftVersion=1.26.45  raknet=11  packets=187
proto 2193  ok    protocol=2193  minecraftVersion=1.26.50  raknet=11  packets=187

PaletteProbe
proto 1001  ok    states=16913  air=13094
proto 2168  ok    states=17499  air=13629
proto 2193  ok    states=22091  air=17025

MappingProbe
1001 -> 2193  states=16913  exact=16329  reduced=72    fallback=512   (96.97% exact)
2168 -> 2193  states=17499  exact=16906  reduced=73    fallback=520   (97.03% exact)
2193 -> 1001  states=22091  exact=16329  reduced=3712  fallback=2050  (90.72% exact)
2193 -> 2168  states=22091  exact=16906  reduced=3768  fallback=1417  (93.59% exact)
```

The palette sizes are identical to the Go side — 16913 / 17499 / 22091 — which
is the evidence that the hash is byte-exact across the two implementations. The
fallback counts are what the Go relay reports for the same pairs.

Downward mapping (2193 to an older protocol) has a much higher fallback rate
than upward mapping. That is expected and not a bug: 1.26.50 added roughly
4 600 states that simply do not exist in older registries. They become
`info_update` because inventing a substitute would be worse.

## Why the rest is not written

The remaining layers are not blocked on design, they are blocked on effort and
on verification:

- **NetherNet over WebRTC** is the big one. `webrtc-java` 0.20.0 publishes
  native jars for all six platforms this repo targets, so a port is possible;
  `third_party/go-nethernet` (8 129 lines of Go) is the reference. Two
  independent Java NetherNet implementations already exist upstream, so the
  approach is not speculative.
- **The spawn gate** is the subtle part and it is subtle for a reason that does
  not care what language you are in: the client gives up roughly 10 seconds
  after connect, the host's `StartGame` blocks until the client acknowledges
  world load, and the client cannot acknowledge until the chunk burst has been
  captured, translated and replayed. Any port has to reproduce that ordering.
- **Chunk translation** is where the real compatibility risk lives, and it is
  the layer that needs a live client to test at all.

Writing these without the ability to run them against a real client would
produce code that looks finished and is not. The Go relay is the tested
implementation; treat this tree as a verified foundation for a port, not as a
second product.

## Layout

```
build.sh                        compile + probe, no build tool required
lib/                            vendored jars + deps.sha256
tools/resolve_deps.py           re-resolve or verify dependencies
src/main/java/xvrelay/block/    StateHash, Palette, BlockMapper
src/tools/java/xvrelay/tools/   CodecProbe, PaletteProbe, MappingProbe
```

The probes read the pinned palettes from `../internal/blocktranslate/data`, so
this tree expects to live inside the relay repository rather than stand alone.

## License

Same as the rest of the repository: MIT, Copyright (c) 2026 ILwHee.
