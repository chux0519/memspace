// MemSpace glue: data and configuration hooks for the vendored simple
// tokenizer. Not part of upstream simple.
#include <cstddef>
#include <string>

#include "simple_tokenizer.h"

extern "C" {
const char *memspace_pinyin_data = "";
size_t memspace_pinyin_len = 0;

// The bytes must stay valid for the life of the process.
void memspace_set_pinyin(const char *data, size_t len) {
  memspace_pinyin_data = data;
  memspace_pinyin_len = len;
}

// dir must end with a path separator; cppjieba concatenates file names.
// Takes effect only before the first jieba_query (the segmenter is a
// process-wide singleton).
void memspace_set_jieba_dict(const char *dir) { simple_tokenizer::jieba_dict_path = dir; }
}
