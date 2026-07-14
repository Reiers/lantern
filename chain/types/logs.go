// Copied from github.com/filecoin-project/lotus/chain/types/logs.go
// at commit a0ecb8687f1c60d5e66040b6de364dbc9cc4d253. See LICENSE.
package types

import (
	"github.com/ipfs/go-cid"
	"go.uber.org/zap/zapcore"
)

type LogCids []cid.Cid

var _ zapcore.ArrayMarshaler = (*LogCids)(nil)

func (cids LogCids) MarshalLogArray(ae zapcore.ArrayEncoder) error {
	for _, c := range cids {
		ae.AppendString(c.String())
	}
	return nil
}
