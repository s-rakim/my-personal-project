#include "bswisp/json.hpp"

#include <array>
#include <cmath>
#include <cstdio>
#include <cstdlib>

namespace bswisp::json {
namespace {

class Parser {
 public:
  explicit Parser(std::string_view text) : s_(text) {}

  Value parseDocument() {
    skipWhitespace();
    Value v = parseValue(0);
    skipWhitespace();
    if (pos_ != s_.size()) {
      throw ParseError("trailing content after document", pos_);
    }
    return v;
  }

 private:
  // kMaxDepth stops a hostile or corrupt payload from exhausting the stack
  // through nesting alone. The solver's own documents nest four levels deep.
  static constexpr int kMaxDepth = 200;

  Value parseValue(int depth) {
    if (depth > kMaxDepth) throw ParseError("nesting too deep", pos_);
    if (pos_ >= s_.size()) throw ParseError("unexpected end of input", pos_);

    switch (s_[pos_]) {
      case '{': return parseObject(depth);
      case '[': return parseArray(depth);
      case '"': return Value(parseString());
      case 't': expect("true"); return Value(true);
      case 'f': expect("false"); return Value(false);
      case 'n': expect("null"); return Value(nullptr);
      default: return parseNumber();
    }
  }

  Value parseObject(int depth) {
    ++pos_;  // '{'
    Value out = Value::object();
    skipWhitespace();
    if (peek() == '}') {
      ++pos_;
      return out;
    }
    for (;;) {
      skipWhitespace();
      if (peek() != '"') throw ParseError("expected object key", pos_);
      std::string key = parseString();
      skipWhitespace();
      if (peek() != ':') throw ParseError("expected ':' after object key", pos_);
      ++pos_;
      skipWhitespace();
      out.set(std::move(key), parseValue(depth + 1));
      skipWhitespace();
      char c = peek();
      if (c == ',') {
        ++pos_;
        continue;
      }
      if (c == '}') {
        ++pos_;
        return out;
      }
      throw ParseError("expected ',' or '}' in object", pos_);
    }
  }

  Value parseArray(int depth) {
    ++pos_;  // '['
    Value out = Value::array();
    skipWhitespace();
    if (peek() == ']') {
      ++pos_;
      return out;
    }
    for (;;) {
      skipWhitespace();
      out.push(parseValue(depth + 1));
      skipWhitespace();
      char c = peek();
      if (c == ',') {
        ++pos_;
        continue;
      }
      if (c == ']') {
        ++pos_;
        return out;
      }
      throw ParseError("expected ',' or ']' in array", pos_);
    }
  }

  std::string parseString() {
    ++pos_;  // opening quote
    std::string out;
    for (;;) {
      if (pos_ >= s_.size()) throw ParseError("unterminated string", pos_);
      char c = s_[pos_];
      if (c == '"') {
        ++pos_;
        return out;
      }
      if (c != '\\') {
        // Control characters must be escaped per RFC 8259.
        if (static_cast<unsigned char>(c) < 0x20) {
          throw ParseError("unescaped control character in string", pos_);
        }
        out.push_back(c);
        ++pos_;
        continue;
      }

      ++pos_;  // backslash
      if (pos_ >= s_.size()) throw ParseError("unterminated escape", pos_);
      char esc = s_[pos_++];
      switch (esc) {
        case '"': out.push_back('"'); break;
        case '\\': out.push_back('\\'); break;
        case '/': out.push_back('/'); break;
        case 'b': out.push_back('\b'); break;
        case 'f': out.push_back('\f'); break;
        case 'n': out.push_back('\n'); break;
        case 'r': out.push_back('\r'); break;
        case 't': out.push_back('\t'); break;
        case 'u': appendUnicodeEscape(out); break;
        default: throw ParseError("unknown escape sequence", pos_ - 1);
      }
    }
  }

  // appendUnicodeEscape decodes \uXXXX, joining a surrogate pair when it sees
  // one, and emits UTF-8. A lone surrogate is replaced rather than rejected:
  // dropping a whole plan over one bad character in a subscriber's name would
  // be a worse outcome than mangling that name.
  void appendUnicodeEscape(std::string& out) {
    unsigned cp = readHex4();
    if (cp >= 0xD800 && cp <= 0xDBFF) {
      if (pos_ + 1 < s_.size() && s_[pos_] == '\\' && s_[pos_ + 1] == 'u') {
        std::size_t save = pos_;
        pos_ += 2;
        unsigned lo = readHex4();
        if (lo >= 0xDC00 && lo <= 0xDFFF) {
          cp = 0x10000 + ((cp - 0xD800) << 10) + (lo - 0xDC00);
        } else {
          pos_ = save;
          cp = 0xFFFD;
        }
      } else {
        cp = 0xFFFD;
      }
    } else if (cp >= 0xDC00 && cp <= 0xDFFF) {
      cp = 0xFFFD;
    }
    appendUTF8(out, cp);
  }

  unsigned readHex4() {
    if (pos_ + 4 > s_.size()) throw ParseError("truncated \\u escape", pos_);
    unsigned v = 0;
    for (int i = 0; i < 4; ++i) {
      char c = s_[pos_++];
      v <<= 4;
      if (c >= '0' && c <= '9') {
        v |= static_cast<unsigned>(c - '0');
      } else if (c >= 'a' && c <= 'f') {
        v |= static_cast<unsigned>(c - 'a' + 10);
      } else if (c >= 'A' && c <= 'F') {
        v |= static_cast<unsigned>(c - 'A' + 10);
      } else {
        throw ParseError("invalid hex digit in \\u escape", pos_ - 1);
      }
    }
    return v;
  }

  static void appendUTF8(std::string& out, unsigned cp) {
    if (cp < 0x80) {
      out.push_back(static_cast<char>(cp));
    } else if (cp < 0x800) {
      out.push_back(static_cast<char>(0xC0 | (cp >> 6)));
      out.push_back(static_cast<char>(0x80 | (cp & 0x3F)));
    } else if (cp < 0x10000) {
      out.push_back(static_cast<char>(0xE0 | (cp >> 12)));
      out.push_back(static_cast<char>(0x80 | ((cp >> 6) & 0x3F)));
      out.push_back(static_cast<char>(0x80 | (cp & 0x3F)));
    } else {
      out.push_back(static_cast<char>(0xF0 | (cp >> 18)));
      out.push_back(static_cast<char>(0x80 | ((cp >> 12) & 0x3F)));
      out.push_back(static_cast<char>(0x80 | ((cp >> 6) & 0x3F)));
      out.push_back(static_cast<char>(0x80 | (cp & 0x3F)));
    }
  }

  Value parseNumber() {
    std::size_t start = pos_;
    if (peek() == '-') ++pos_;
    if (pos_ >= s_.size()) throw ParseError("truncated number", pos_);

    if (s_[pos_] == '0') {
      ++pos_;
    } else if (s_[pos_] >= '1' && s_[pos_] <= '9') {
      while (pos_ < s_.size() && isDigit(s_[pos_])) ++pos_;
    } else {
      throw ParseError("invalid number", pos_);
    }

    if (pos_ < s_.size() && s_[pos_] == '.') {
      ++pos_;
      if (pos_ >= s_.size() || !isDigit(s_[pos_])) {
        throw ParseError("expected digit after decimal point", pos_);
      }
      while (pos_ < s_.size() && isDigit(s_[pos_])) ++pos_;
    }

    if (pos_ < s_.size() && (s_[pos_] == 'e' || s_[pos_] == 'E')) {
      ++pos_;
      if (pos_ < s_.size() && (s_[pos_] == '+' || s_[pos_] == '-')) ++pos_;
      if (pos_ >= s_.size() || !isDigit(s_[pos_])) {
        throw ParseError("expected digit in exponent", pos_);
      }
      while (pos_ < s_.size() && isDigit(s_[pos_])) ++pos_;
    }

    std::string text(s_.substr(start, pos_ - start));
    return Value(std::strtod(text.c_str(), nullptr));
  }

  void expect(std::string_view literal) {
    if (s_.compare(pos_, literal.size(), literal) != 0) {
      throw ParseError("invalid literal", pos_);
    }
    pos_ += literal.size();
  }

  void skipWhitespace() {
    while (pos_ < s_.size()) {
      char c = s_[pos_];
      if (c == ' ' || c == '\t' || c == '\n' || c == '\r') {
        ++pos_;
      } else {
        break;
      }
    }
  }

  char peek() const { return pos_ < s_.size() ? s_[pos_] : '\0'; }
  static bool isDigit(char c) { return c >= '0' && c <= '9'; }

  std::string_view s_;
  std::size_t pos_ = 0;
};

void escapeInto(std::string& out, const std::string& s) {
  out.push_back('"');
  for (char c : s) {
    switch (c) {
      case '"': out += "\\\""; break;
      case '\\': out += "\\\\"; break;
      case '\b': out += "\\b"; break;
      case '\f': out += "\\f"; break;
      case '\n': out += "\\n"; break;
      case '\r': out += "\\r"; break;
      case '\t': out += "\\t"; break;
      default:
        if (static_cast<unsigned char>(c) < 0x20) {
          std::array<char, 8> buf{};
          std::snprintf(buf.data(), buf.size(), "\\u%04x",
                        static_cast<unsigned>(static_cast<unsigned char>(c)));
          out += buf.data();
        } else {
          out.push_back(c);
        }
    }
  }
  out.push_back('"');
}

// numberInto emits integral doubles without a decimal point and everything else
// with enough precision to survive a round trip. Non-finite values are written
// as null, since JSON has no NaN and a silent 0 would be read as a real
// measurement.
void numberInto(std::string& out, double v) {
  if (!std::isfinite(v)) {
    out += "null";
    return;
  }
  if (v == std::floor(v) && std::fabs(v) < 1e15) {
    std::array<char, 32> buf{};
    std::snprintf(buf.data(), buf.size(), "%lld", static_cast<long long>(v));
    out += buf.data();
    return;
  }
  std::array<char, 40> buf{};
  std::snprintf(buf.data(), buf.size(), "%.17g", v);
  // %.17g always round-trips but is ugly. Try shorter forms and keep the first
  // that reads back identically.
  for (int precision = 6; precision < 17; ++precision) {
    std::array<char, 40> candidate{};
    std::snprintf(candidate.data(), candidate.size(), "%.*g", precision, v);
    if (std::strtod(candidate.data(), nullptr) == v) {
      out += candidate.data();
      return;
    }
  }
  out += buf.data();
}

void dumpInto(std::string& out, const Value& v, int indent, int depth) {
  const bool pretty = indent >= 0;
  auto newlineIndent = [&](int d) {
    if (!pretty) return;
    out.push_back('\n');
    out.append(static_cast<std::size_t>(indent * d), ' ');
  };

  switch (v.type()) {
    case Value::Type::Null:
      out += "null";
      return;
    case Value::Type::Bool:
      out += v.asBool() ? "true" : "false";
      return;
    case Value::Type::Number:
      numberInto(out, v.asNumber());
      return;
    case Value::Type::String:
      escapeInto(out, v.asString());
      return;
    case Value::Type::Array: {
      if (v.items().empty()) {
        out += "[]";
        return;
      }
      out.push_back('[');
      bool first = true;
      for (const auto& item : v.items()) {
        if (!first) out.push_back(',');
        first = false;
        newlineIndent(depth + 1);
        dumpInto(out, item, indent, depth + 1);
      }
      newlineIndent(depth);
      out.push_back(']');
      return;
    }
    case Value::Type::Object: {
      if (v.members().empty()) {
        out += "{}";
        return;
      }
      out.push_back('{');
      bool first = true;
      for (const auto& m : v.members()) {
        if (!first) out.push_back(',');
        first = false;
        newlineIndent(depth + 1);
        escapeInto(out, m.first);
        out.push_back(':');
        if (pretty) out.push_back(' ');
        dumpInto(out, m.second, indent, depth + 1);
      }
      newlineIndent(depth);
      out.push_back('}');
      return;
    }
  }
}

}  // namespace

Value parse(std::string_view text) { return Parser(text).parseDocument(); }

std::string dump(const Value& v, int indent) {
  std::string out;
  out.reserve(1024);
  dumpInto(out, v, indent, 0);
  return out;
}

}  // namespace bswisp::json
