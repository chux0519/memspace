# Vendored: sqlite-vec

| Component | Source | Version | License |
|---|---|---|---|
| `sqlite-vec.c`, `sqlite-vec.h` (amalgamation) | https://github.com/asg017/sqlite-vec | v0.1.9 (e9f598a, 2026-03-31) | MIT or Apache-2.0 (`LICENSE-MIT`, `LICENSE-APACHE`) |

Compiled into MemSpace via cgo with `SQLITE_CORE` (linked against the SQLite
in mattn/go-sqlite3; headers from `../fts/simple/include`) and registered
with `sqlite3_auto_extension`. NEON is enabled on arm64 (always available);
AVX is not enabled on amd64, to keep binaries portable.

Local change: the NEON int8 L1 distance (`l1_int8_neon`) casts between
signed and unsigned vectors explicitly (`vreinterpretq_u8_s8`,
`vreinterpretq_s32_u32`); GCC on Linux arm64 rejects the implicit
conversions that clang accepts. The arithmetic is unchanged.
