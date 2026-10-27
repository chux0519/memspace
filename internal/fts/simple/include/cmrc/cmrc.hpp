// Minimal stand-in for CMakeRC (https://github.com/vector-of-bool/cmrc), which
// upstream simple uses to embed pinyin.txt. MemSpace embeds the file in Go and
// hands the bytes to C++ through memspace_set_pinyin() before first use.
#pragma once
#include <cstddef>
#include <string>

extern "C" const char *memspace_pinyin_data;
extern "C" size_t memspace_pinyin_len;

namespace cmrc {
struct file {
  const char *b;
  const char *e;
  const char *begin() const { return b; }
  const char *end() const { return e; }
};
struct embedded_filesystem {
  file open(const std::string &) const { return {memspace_pinyin_data, memspace_pinyin_data + memspace_pinyin_len}; }
};
}  // namespace cmrc

#define CMRC_DECLARE(ns)                                         \
  namespace cmrc {                                               \
  namespace ns {                                                 \
  inline embedded_filesystem get_filesystem() { return {}; }    \
  }                                                              \
  }
