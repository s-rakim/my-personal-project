// Minimal JSON value, parser and serialiser.
//
// Written here rather than vendored so the solver builds on a bare box with
// nothing but a C++17 compiler. The payloads are configuration-sized (tens of
// thousands of values), so clarity beats micro-optimisation, but the parser is
// still single-pass and allocation-light enough to run inside a 15-second tick
// budget with room to spare.
#ifndef BSWISP_JSON_HPP
#define BSWISP_JSON_HPP

#include <cstddef>
#include <initializer_list>
#include <stdexcept>
#include <string>
#include <string_view>
#include <utility>
#include <vector>

namespace bswisp::json {

// ParseError carries the byte offset so a malformed payload can be pinpointed
// rather than merely rejected.
class ParseError : public std::runtime_error {
 public:
  ParseError(std::string message, std::size_t offset)
      : std::runtime_error(std::move(message) + " at offset " + std::to_string(offset)),
        offset_(offset) {}

  std::size_t offset() const noexcept { return offset_; }

 private:
  std::size_t offset_;
};

class Value;
using Member = std::pair<std::string, Value>;

// Value is a JSON node. Objects keep insertion order, which makes emitted
// documents stable and therefore diffable in the conformance test.
class Value {
 public:
  enum class Type { Null, Bool, Number, String, Array, Object };

  Value() = default;
  Value(std::nullptr_t) : type_(Type::Null) {}
  Value(bool b) : type_(Type::Bool), bool_(b) {}
  Value(double n) : type_(Type::Number), num_(n) {}
  Value(int n) : type_(Type::Number), num_(static_cast<double>(n)) {}
  Value(long long n) : type_(Type::Number), num_(static_cast<double>(n)) {}
  Value(std::string s) : type_(Type::String), str_(std::move(s)) {}
  Value(const char* s) : type_(Type::String), str_(s) {}

  static Value array() {
    Value v;
    v.type_ = Type::Array;
    return v;
  }

  static Value object() {
    Value v;
    v.type_ = Type::Object;
    return v;
  }

  Type type() const noexcept { return type_; }
  bool isNull() const noexcept { return type_ == Type::Null; }
  bool isBool() const noexcept { return type_ == Type::Bool; }
  bool isNumber() const noexcept { return type_ == Type::Number; }
  bool isString() const noexcept { return type_ == Type::String; }
  bool isArray() const noexcept { return type_ == Type::Array; }
  bool isObject() const noexcept { return type_ == Type::Object; }

  // Typed accessors return a caller-supplied fallback rather than throwing.
  // A field absent from a peer's payload is the normal case under the additive
  // schema rules, not an exception.
  bool asBool(bool fallback = false) const noexcept {
    return type_ == Type::Bool ? bool_ : fallback;
  }

  double asNumber(double fallback = 0.0) const noexcept {
    return type_ == Type::Number ? num_ : fallback;
  }

  long long asInt(long long fallback = 0) const noexcept {
    return type_ == Type::Number ? static_cast<long long>(num_) : fallback;
  }

  std::string asString(std::string fallback = {}) const {
    return type_ == Type::String ? str_ : fallback;
  }

  std::size_t size() const noexcept {
    if (type_ == Type::Array) return arr_.size();
    if (type_ == Type::Object) return obj_.size();
    return 0;
  }

  const std::vector<Value>& items() const noexcept { return arr_; }
  const std::vector<Member>& members() const noexcept { return obj_; }

  void push(Value v) {
    if (type_ != Type::Array) type_ = Type::Array;
    arr_.push_back(std::move(v));
  }

  // Object access. Linear lookup is deliberate: objects here hold a handful of
  // keys, and a vector beats a hash map at that size while keeping order.
  bool has(std::string_view key) const noexcept { return find(key) != nullptr; }

  const Value& operator[](std::string_view key) const noexcept {
    static const Value kNull;
    const Value* v = find(key);
    return v ? *v : kNull;
  }

  void set(std::string key, Value v) {
    if (type_ != Type::Object) type_ = Type::Object;
    for (auto& m : obj_) {
      if (m.first == key) {
        m.second = std::move(v);
        return;
      }
    }
    obj_.emplace_back(std::move(key), std::move(v));
  }

 private:
  const Value* find(std::string_view key) const noexcept {
    if (type_ != Type::Object) return nullptr;
    for (const auto& m : obj_) {
      if (m.first == key) return &m.second;
    }
    return nullptr;
  }

  Type type_ = Type::Null;
  bool bool_ = false;
  double num_ = 0.0;
  std::string str_;
  std::vector<Value> arr_;
  std::vector<Member> obj_;
};

// parse decodes a complete JSON document. Throws ParseError.
Value parse(std::string_view text);

// dump serialises a value. indent < 0 produces compact output; indent >= 0
// pretty-prints with that many spaces per level.
std::string dump(const Value& v, int indent = -1);

}  // namespace bswisp::json

#endif  // BSWISP_JSON_HPP
