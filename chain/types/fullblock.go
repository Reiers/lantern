// Copied from github.com/filecoin-project/lotus/chain/types/fullblock.go
// at commit a0ecb8687f1c60d5e66040b6de364dbc9cc4d253. See LICENSE.
package types

import "github.com/ipfs/go-cid"

type FullBlock struct {
	Header        *BlockHeader
	BlsMessages   []*Message
	SecpkMessages []*SignedMessage
}

func (fb *FullBlock) Cid() cid.Cid {
	return fb.Header.Cid()
}
