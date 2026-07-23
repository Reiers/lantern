// refgen regenerates the reference reorg-probability vectors used by
// TestCalcValidatorProb_PythonReference against the calculator's current
// implementation on the CPU it runs on.
//
// Usage:
//
//	go run ./chain/ecfinality/tools/refgen
//
// Copy the printed map body into calculator_test.go under the appropriate
// reference vector (pythonReferenceResults for FMA-capable CPUs,
// nonFMAReferenceResults for x86_64 without FMA3). This is only needed
// when adding support for a new CPU class or when the algorithm's math
// changes intentionally.
//
// Background: math.Log / math.Exp / math.Lgamma produce slightly
// different values on FMA-capable vs non-FMA CPUs due to different
// polynomial evaluations. The test dispatches by cpu.X86.HasFMA and
// checks 1e-12 parity within each class, so regressions on either CPU
// family are caught cleanly without loosening tolerance globally.
package main

import (
	"fmt"

	"golang.org/x/sys/cpu"

	ec "github.com/Reiers/lantern/chain/ecfinality"
)

// referenceChain matches pythonReferenceChain in calculator_test.go.
// Duplicated here (rather than exported) because the test data is
// deliberately package-private, and this tool is a one-off generator.
// Generated in Python with: numpy.random.default_rng(0).poisson(4.5, 905).
var referenceChain = []int{
	2, 3, 4, 3, 4, 7, 5, 6, 5, 5, 4, 2, 3, 3, 10, 7, 3, 8, 6, 3,
	2, 3, 5, 3, 7, 6, 5, 3, 4, 6, 6, 8, 6, 3, 2, 6, 5, 2, 4, 4,
	4, 6, 5, 7, 8, 6, 3, 0, 10, 8, 3, 7, 4, 6, 4, 6, 5, 2, 5, 5,
	7, 6, 2, 1, 3, 5, 3, 5, 10, 4, 0, 5, 11, 6, 8, 6, 4, 8, 3, 4,
	3, 2, 5, 6, 6, 5, 3, 9, 5, 2, 9, 3, 6, 5, 4, 6, 2, 3, 4, 7,
	5, 8, 2, 6, 0, 3, 5, 6, 6, 4, 3, 6, 5, 2, 3, 4, 6, 1, 5, 3,
	5, 7, 2, 4, 11, 3, 4, 8, 5, 3, 6, 6, 7, 5, 1, 2, 1, 4, 4, 5,
	6, 4, 2, 6, 5, 5, 1, 2, 5, 5, 0, 4, 4, 7, 4, 10, 6, 4, 9, 5,
	5, 1, 0, 3, 7, 1, 6, 4, 3, 5, 7, 6, 10, 3, 5, 4, 1, 6, 2, 2,
	2, 5, 4, 7, 4, 2, 5, 6, 3, 8, 4, 6, 6, 5, 3, 3, 3, 2, 5, 5,
	7, 3, 4, 6, 7, 5, 3, 4, 5, 7, 6, 3, 5, 8, 6, 2, 5, 3, 4, 4,
	7, 4, 3, 8, 3, 4, 3, 5, 4, 7, 8, 8, 4, 4, 5, 5, 4, 9, 5, 3,
	4, 6, 4, 4, 4, 3, 5, 5, 3, 8, 3, 4, 4, 6, 8, 3, 6, 4, 5, 6,
	7, 4, 4, 3, 9, 6, 4, 8, 6, 4, 6, 8, 5, 5, 5, 5, 4, 5, 6, 3,
	3, 8, 3, 3, 3, 3, 8, 3, 2, 8, 4, 6, 5, 2, 3, 2, 5, 3, 7, 4,
	3, 4, 5, 4, 3, 7, 6, 6, 5, 3, 3, 6, 5, 4, 3, 5, 7, 5, 4, 5,
	6, 4, 6, 5, 3, 3, 4, 5, 6, 5, 5, 8, 3, 5, 6, 6, 3, 3, 6, 5,
	9, 5, 3, 4, 4, 4, 3, 4, 3, 6, 4, 5, 8, 5, 4, 7, 2, 4, 6, 3,
	3, 3, 9, 7, 3, 5, 5, 6, 6, 3, 3, 6, 4, 3, 6, 5, 3, 7, 4, 5,
	5, 4, 6, 4, 5, 6, 5, 6, 4, 5, 6, 4, 3, 5, 6, 4, 5, 7, 4, 4,
	3, 6, 5, 8, 4, 3, 4, 4, 7, 5, 4, 5, 6, 5, 2, 4, 3, 4, 4, 4,
	7, 5, 3, 5, 6, 4, 4, 7, 4, 6, 8, 8, 4, 5, 3, 6, 3, 4, 8, 4,
	3, 4, 3, 6, 4, 6, 4, 8, 4, 5, 5, 7, 3, 3, 3, 4, 4, 4, 5, 5,
	5, 3, 6, 5, 5, 2, 6, 1, 11, 3, 3, 5, 5, 6, 2, 5, 3, 4, 5, 5,
	7, 7, 7, 9, 3, 4, 6, 3, 3, 2, 6, 6, 1, 3, 1, 5, 7, 5, 7, 8,
	4, 5, 2, 6, 6, 5, 7, 5, 5, 6, 4, 2, 7, 6, 5, 5, 9, 4, 3, 3,
	1, 1, 4, 5, 5, 6, 7, 2, 4, 6, 3, 5, 5, 5, 4, 2, 4, 3, 3, 5,
	2, 4, 4, 5, 6, 3, 6, 4, 5, 4, 5, 2, 8, 6, 5, 6, 7, 6, 2, 4,
	9, 1, 3, 5, 4, 7, 2, 5, 4, 7, 9, 2, 3, 2, 2, 7, 4, 1, 2, 6,
	5, 10, 2, 4, 3,
}

func main() {
	fmt.Printf("// CPU features: FMA=%v AVX2=%v AVX=%v\n",
		cpu.X86.HasFMA, cpu.X86.HasAVX2, cpu.X86.HasAVX)
	fmt.Println()

	current := len(referenceChain) - 1
	depths := []int{5, 10, 15, 20, 25, 30, 40, 50, 75, 100}

	fmt.Println("// paste this into the appropriate reference map:")
	for _, depth := range depths {
		target := current - depth
		p := ec.CalcValidatorProb(referenceChain, 900, 5.0, 0.3, current, target)
		fmt.Printf("\t%-3d: %.20e,\n", depth, p)
	}
}
