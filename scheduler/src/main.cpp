// bswisp-solver: reads a SchedulerProblem on stdin, writes a SchedulerPlan on
// stdout.
//
// A subprocess with a JSON contract, rather than a library linked into the
// control plane, for three reasons: the solver can be run by hand against a
// captured problem when a subscriber complains, a crash in it cannot take the
// AAA server down with it, and it can be rewritten in anything later without
// touching the daemon.
#include <cstdio>
#include <fstream>
#include <iostream>
#include <iterator>
#include <sstream>
#include <string>

#include "bswisp/airtime.hpp"
#include "bswisp/json.hpp"
#include "bswisp/model.hpp"

namespace {

constexpr int kExitOK = 0;
constexpr int kExitUsage = 1;
constexpr int kExitBadInput = 2;
constexpr int kExitIO = 3;

void usage() {
  std::cerr <<
      "bswisp-solver -- airtime scheduler for fixed wireless sectors\n"
      "\n"
      "usage: bswisp-solver [options]\n"
      "\n"
      "  --in FILE     read the problem from FILE (default: stdin)\n"
      "  --out FILE    write the plan to FILE (default: stdout)\n"
      "  --pretty      indent the output, for reading by humans\n"
      "  --version     print the solver version and exit\n"
      "  -h, --help    this text\n"
      "\n"
      "The problem and plan formats are schema/scheduler-problem.schema.json\n"
      "and schema/scheduler-plan.schema.json.\n";
}

std::string readAll(std::istream& in) {
  return std::string(std::istreambuf_iterator<char>(in), std::istreambuf_iterator<char>());
}

}  // namespace

int main(int argc, char** argv) {
  std::string inPath, outPath;
  bool pretty = false;

  for (int i = 1; i < argc; ++i) {
    const std::string arg = argv[i];
    if (arg == "--pretty") {
      pretty = true;
    } else if (arg == "--version") {
      std::cout << bswisp::kSolverVersion << "\n";
      return kExitOK;
    } else if (arg == "-h" || arg == "--help") {
      usage();
      return kExitOK;
    } else if (arg == "--in" && i + 1 < argc) {
      inPath = argv[++i];
    } else if (arg == "--out" && i + 1 < argc) {
      outPath = argv[++i];
    } else {
      std::cerr << "bswisp-solver: unrecognised argument: " << arg << "\n\n";
      usage();
      return kExitUsage;
    }
  }

  std::string text;
  if (inPath.empty()) {
    text = readAll(std::cin);
  } else {
    std::ifstream f(inPath, std::ios::binary);
    if (!f) {
      std::cerr << "bswisp-solver: cannot open " << inPath << "\n";
      return kExitIO;
    }
    text = readAll(f);
  }

  if (text.find_first_not_of(" \t\r\n") == std::string::npos) {
    std::cerr << "bswisp-solver: empty input\n";
    return kExitBadInput;
  }

  bswisp::Plan plan;
  try {
    const bswisp::json::Value doc = bswisp::json::parse(text);
    const bswisp::Problem problem = bswisp::Problem::fromJson(doc);
    plan = bswisp::AirtimeScheduler().Solve(problem);
  } catch (const bswisp::json::ParseError& e) {
    std::cerr << "bswisp-solver: malformed problem: " << e.what() << "\n";
    return kExitBadInput;
  } catch (const std::exception& e) {
    std::cerr << "bswisp-solver: " << e.what() << "\n";
    return kExitBadInput;
  }

  const std::string rendered = bswisp::json::dump(plan.toJson(), pretty ? 2 : -1);

  if (outPath.empty()) {
    std::cout << rendered << "\n";
  } else {
    std::ofstream f(outPath, std::ios::binary | std::ios::trunc);
    if (!f) {
      std::cerr << "bswisp-solver: cannot write " << outPath << "\n";
      return kExitIO;
    }
    f << rendered << "\n";
    if (!f) {
      std::cerr << "bswisp-solver: write to " << outPath << " failed\n";
      return kExitIO;
    }
  }
  return kExitOK;
}
