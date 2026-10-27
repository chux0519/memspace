# Vendored: simple FTS5 tokenizer + cppjieba

Statically compiled into MemSpace (cgo) so Chinese full-text search needs no
loadable extension and no system libraries.

| Component | Source | Version | License |
|---|---|---|---|
| simple (SQLite FTS5 tokenizer, jieba_query, simple_highlight) | https://github.com/wangfenjin/simple | 45db071 (2026-05-17) | MIT (dual MIT/GPL-3.0, MIT chosen), `LICENSE-simple` |
| cppjieba + limonp (header-only) | https://github.com/yanyiwu/cppjieba | 194c144 (the commit simple pins) | MIT, `LICENSE-cppjieba` |
| jieba dictionaries (`data/*.utf8.gz`) | cppjieba `dict/` | same | MIT |
| pinyin table (`data/pinyin.txt.gz`) | simple `contrib/pinyin.txt` | same | MIT |
| `include/sqlite3-binding.h`, `include/sqlite3ext.h` | mattn/go-sqlite3 | v1.14.52 | public domain |

Local changes:

- `include/cmrc/cmrc.hpp` replaces CMakeRC; `memspace_glue.cc` supplies the
  embedded pinyin bytes and the jieba dictionary directory from Go.
- `data/user.dict.utf8` is MemSpace's technical-term dictionary (not upstream).
- `include/limonp/StringUtil.hpp`: the trim helpers use lambdas instead of
  `std::ptr_fun` / `std::bind2nd`, which C++17 removed (libc++ on macOS
  rejects them).

Upstream source files are otherwise unmodified. When updating, keep the
SQLite headers in sync with the mattn/go-sqlite3 version in go.mod.
