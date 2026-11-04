> **Note:** this document was written while the tool was still called `xver`. The old name is kept
> AS-IS throughout (`cmd/xver`, the `xver-*.exe` binaries, `log/xver-*.log`) so it matches the logs
> that were actually measured. The tool has since been renamed to `xvrelay`; current source lives
> in `cmd/xvrelay`.

# xver compatibility matrix — 2026-10-26

> **Correction, measured 2026-11-05:** the matrix now runs **8** checks per pair, not 7, so the
> total is **16 × 8 = 128**. The extra check is `UpdateBlock` codec→relay→codec round-tripping,
> which is present in the test but missing from the list below. Re-run on a clean tree:
> `checks=8 findings=[]` for all 16 pairs, `dist/matrix.json` reports `128` total and `0` findings.
> The `112` figures further down are the 2026-10-26 measurement and are kept as written.

## Update after the fix

The findings below have been fixed. Post-fix matrix: **112/112 checks PASS, 16/16 combinations
report no finding in the checked group**. JSON/TXT results in
`log/protocol-matrix-fixed-2026-10-26.*`. The tables and findings further down are kept as the
pre-fix results; they are not the current state.

Changes in `cmd/xver/main.go`:
- `legacy1001LevelChunkPacket` preserves payload/cache/hash; a finite limit uses the sentinel
  `0xfffffffe` with a `uint16`, unlimited uses `0xffffffff`. Tested with `-1/0/9/65535`, with and
  without cache.
- `rawForward` decodes `StartGame` with the host codec and re-encodes it with the target codec when
  the `GameVersion` string changes. It no longer drops an old protocol merely because it differs
  from the native version.
- `startGameFromHostFor1001` takes `hostProtocol` so it picks the right decoder when the host is
  2168/2169.

Regression tests: `cmd/xver/legacy_preservation_test.go`. The tests were observed failing before the
patch and passing after it. Default suite `cmd/xver` + `internal/relay` + `internal/lansignal` and
`go vet cmd/xver` pass. Separate release build for this round: `xver-matrix-fixed.exe`.

Real Minecraft was **not** run for all 16 pairs after the fix. The matrix only proves the invariants
that were checked; it is not a promise that every registry/addon/gameplay path is compatible. A
running relay is never stopped by this procedure.

Scope: the 16 protocol pairs among 1001/2168/2169/2193 that `resolveProto` supports, 7 checks per
pair = 112. This is wire/model/relay verification using fixtures plus a RakNet loopback connection,
**not** 16 real Minecraft sessions. Protocol 1001 stands for the 1.26.30–36 group (including
1.26.33); 2168 for 1.26.40–44; 2169 for 1.26.45; 2193 for 1.26.50–51. Never infer support from a
version number alone.

| Client → host | 1001 | 2168 | 2169 | 2193 |
|---|---|---|---|---|
| 1001 | M+L | L | L | L |
| 2168 | T | M | T | T |
| 2169 | T | T | M | T |
| 2193 | R | T | T | T |

T: no error found in the 7 checks. L: the request-mode `LevelChunk` has its payload stripped.
M: `LevelID`/`ServerID`/`ServerChunkTickRadius` lost from `StartGame` on the same protocol.
R: every check passes, and a real Minecraft 1.26.51 once spawned and rendered terrain through a 1001
host; inventory/chat and full block compatibility were not confirmed.

## Evidence
- `cmd/xver/protocol_matrix_test.go`: an opt-in test that collects the report but still fails loudly
  when there is a finding. Client negotiation uses the codec's own `GameVersion` instead of letting
  the `Dialer` default to 1.26.51 for every protocol.
- `log/protocol-matrix-2026-10-26.json`: results for the 16 pairs, 6 pairs with findings, 7 failing
  checks out of 112.
- `log/protocol-matrix-2026-10-26.txt`: test stdout, exit status 1 because the findings reproduce.
- Earlier live run: `log/xver-2026-10-26_19-54-03.log`; Minecraft 2193 through a 1001 host after the
  `DimensionData`/deferred-queue fix showed grass/TNT/chest/player. A screenshot is not treated as
  proof of the whole block palette.

The 7 checks per pair: `StartGame` metadata + rules/experiments/permissions; inline `LevelChunk`;
request-mode `LevelChunk`; `SubChunk` payload + offset; host chat; inventory envelope; client
`SubChunkRequest`. The synthetic chunk payload only checks byte/envelope preservation, not a full
terrain decoder. A test showing the item *count* is unchanged does not prove that item means the
same thing in both versions' registries.

## Verified causes
1. `cmd/xver/main.go` `legacy1001LevelChunkPacket` wiped `RawPayload` whenever `SubChunkLimit` had a
   value, and turned a finite limit into unlimited. This affected all 4 hosts when the client was
   1001.
2. `rawForward` blocked login when `GameVersion` differed from native 1.26.51, and the raw
   `StartGame` recovery path only ran when `clientProto != hostProto`. So the same protocol on an
   old version was rebuilt from `GameData`, losing metadata that `GameData` does not carry.
3. `startGameFromHostFor1001` always used the native decoder — a spot that still needs review for
   2168/2169 hosts. The current fixtures do not reproduce this specific failure, so it is not
   counted as a proven finding.

## Remaining limits and risks
- The wire-only `New()` adapter does not fully configure the registry/block/item tables. The `T`
  cells are not a promise of complete gameplay, nor that item/recipe/every palette entry is right.
- Real Minecraft was not run for the other 15 pairs; the host app's world was not changed and it was
  not restarted during this check.
- Protocols outside the list are rejected: the 20:10 log records client 975 as incompatible; this is
  not treated as support for 1.26.21.
- No product code was changed and no EXE was swapped during this evaluation round. Only a probe test
  was added and the test helper's `GameVersion` fixed.

Re-run from PowerShell in the `bedrock-cross-version-relay` directory:

```powershell
$env:XVER_MATRIX_REPORT = Join-Path (Get-Location) 'log\protocol-matrix-2026-10-26.json'
go test -count=1 -timeout 90s -run TestProtocolCompatibilityMatrix -v ./cmd/xver
Remove-Item Env:XVER_MATRIX_REPORT
```
