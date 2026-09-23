package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	abi "github.com/filecoin-project/go-state-types/abi"

	"github.com/Reiers/lantern/chain/bootstrap"
	"github.com/Reiers/lantern/chain/headcheck"
)

type hcSrc struct{ k bootstrap.Kind }

func (s hcSrc) Name() string                                      { return string(s.k) }
func (s hcSrc) Kind() bootstrap.Kind                              { return s.k }
func (s hcSrc) HeadEpoch(context.Context) (abi.ChainEpoch, error) { return 100, nil }

func TestWriteHeadcheckMetrics(t *testing.T) {
	var buf bytes.Buffer
	writeHeadcheckMetrics(&buf, nil)
	if !strings.Contains(buf.String(), "lantern_headcheck_enabled 0") {
		t.Fatalf("nil monitor must export enabled 0:\n%s", buf.String())
	}

	m := headcheck.New(headcheck.Config{
		Local:   func() abi.ChainEpoch { return 100 },
		Sources: []headcheck.HeadSource{hcSrc{bootstrap.KindForest}, hcSrc{bootstrap.KindUser}},
	})
	m.CheckOnce(context.Background())
	buf.Reset()
	writeHeadcheckMetrics(&buf, m)
	out := buf.String()
	for _, want := range []string{
		"lantern_headcheck_enabled 1",
		`lantern_headcheck_status{status="agree"} 1`,
		`lantern_headcheck_status{status="diverge"} 0`,
		`lantern_headcheck_voters{outcome="agree"} 2`,
		`lantern_headcheck_head_epoch{side="local"} 100`,
		"lantern_headcheck_rounds_total 1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
