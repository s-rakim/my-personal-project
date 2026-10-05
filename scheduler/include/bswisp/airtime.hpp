// The airtime scheduler.
//
// A wireless sector does not have bandwidth to divide. It has one second of
// airtime per second, and every terminal spends that airtime at a different
// exchange rate set by its modulation. A terminal at the cell edge running
// QPSK 1/2 pays eight times as much airtime per megabit as one at close range
// running 1024QAM 5/6. Allocate bandwidth equally and the edge terminals
// silently eat the sector; allocate airtime and the sector behaves.
//
// The algorithm, per sector, is two phases:
//
//   1. Reserve the airtime each terminal needs to hit its committed rate. If
//      the commitments alone exceed the budget, the sector is oversubscribed:
//      scale them back proportionally and say so loudly, because that is a
//      capacity problem no scheduler can solve.
//   2. Share the surplus by weighted max-min fairness. Terminals wanting little
//      are satisfied outright; what they leave over is divided among the rest
//      in proportion to plan weight.
//
// Sectors are solved deepest-site-first so that a relay's load on its parent is
// known before the parent is allocated, with the relay injected into the parent
// sector as a synthetic terminal. That is what physically happens on a relay
// network and it is the part naive models get wrong.
#ifndef BSWISP_AIRTIME_HPP
#define BSWISP_AIRTIME_HPP

#include <string>
#include <vector>

#include "bswisp/model.hpp"

namespace bswisp {

// SolverVersion identifies the implementation in every plan it emits.
extern const char* const kSolverVersion;

// WeightedMaxMinFill distributes `available` across terminals so that each
// receives min(want[i], lambda * weight[i]) for the largest feasible lambda.
// Exposed because it is the one piece of maths here worth testing in isolation.
//
// alloc is resized to want.size(). want and weight must be the same length.
// Returns the amount left unallocated.
double WeightedMaxMinFill(std::vector<double>& alloc, const std::vector<double>& want,
                          const std::vector<double>& weight, double available);

class AirtimeScheduler {
 public:
  // Solve is deterministic: identical input yields a byte-identical plan. A
  // scheduler that does not hold that property cannot be debugged from a
  // captured problem, which is the only way these bugs ever get found.
  Plan Solve(const Problem& problem) const;
};

}  // namespace bswisp

#endif  // BSWISP_AIRTIME_HPP
