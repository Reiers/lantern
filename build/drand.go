// DRAND chain configurations used by Filecoin mainnet, lifted from
// github.com/filecoin-project/lotus/build/buildconstants/drand.go at commit
// a0ecb8687f1c60d5e66040b6de364dbc9cc4d253. See LICENSE-LOTUS.
//
// We keep only the ChainInfoJSON (the trust anchor for verification) and the
// IsChained flag. Server lists and libp2p relays belong in net/hsync (when
// they exist); Phase 1 just verifies entries.

package build

// DrandNetwork identifies one of the drand networks Filecoin has trusted over
// its lifetime.
type DrandNetwork int

const (
	DrandMainnet DrandNetwork = iota + 1 // legacy league-of-entropy chained chain
	DrandTestnet
	_ // historical: DrandDevnet (removed)
	_ // historical: DrandLocalnet (removed)
	DrandIncentinet // legacy incentinet chain
	DrandQuicknet   // post-FIP-0063 unchained chain
)

// DrandChainInfo is the static configuration for one drand network: the
// chain-info JSON blob (carries the group public key, scheme ID and chain
// hash) and whether the chain operates in chained mode.
type DrandChainInfo struct {
	ChainInfoJSON string
	IsChained     bool
}

// DrandConfigs holds the chain-info blobs for every drand network Filecoin
// has trusted. Verbatim from buildconstants.DrandConfigs.
var DrandConfigs = map[DrandNetwork]DrandChainInfo{
	DrandQuicknet: {
		IsChained:     false,
		ChainInfoJSON: `{"public_key":"83cf0f2896adee7eb8b5f01fcad3912212c437e0073e911fb90022d3e760183c8c4b450b6a0a6c3ac6a5776a2d1064510d1fec758c921cc22b0e17e63aaf4bcb5ed66304de9cf809bd274ca73bab4af5a6e9c76a4bc09e76eae8991ef5ece45a","period":3,"genesis_time":1692803367,"hash":"52db9ba70e0cc0f6eaf7803dd07447a1f5477735fd3f661792ba94600c84e971","groupHash":"f477d5c89f21a17c863a7f937c6a6d15859414d2be09cd448d4279af331c5d3e","schemeID":"bls-unchained-g1-rfc9380","metadata":{"beaconID":"quicknet"}}`,
	},
	DrandTestnet: {
		IsChained:     true,
		ChainInfoJSON: `{"public_key":"922a2e93828ff83345bae533f5172669a26c02dc76d6bf59c80892e12ab1455c229211886f35bb56af6d5bea981024df","period":25,"genesis_time":1590445175,"hash":"84b2234fb34e835dccd048255d7ad3194b81af7d978c3bf157e3469592ae4e02","groupHash":"4dd408e5fdff9323c76a9b6f087ba8fdc5a6da907bd9217d9d10f2287d081957"}`,
	},
	DrandIncentinet: {
		IsChained:     true,
		ChainInfoJSON: `{"public_key":"8cad0c72c606ab27d36ee06de1d5b2db1faf92e447025ca37575ab3a8aac2eaae83192f846fc9e158bc738423753d000","period":30,"genesis_time":1595873820,"hash":"80c8b872c714f4c00fdd3daa465d5514049f457f01f85a4caf68cdcd394ba039","groupHash":"d9406aaed487f7af71851b4399448e311f2328923d454e971536c05398ce2d9b"}`,
	},
	DrandMainnet: {
		IsChained:     true,
		ChainInfoJSON: `{"public_key":"868f005eb8e6e4ca0a47c8a77ceaa5309a47978a7c71bc5cce96366b5d7a569937c529eeda66c7293784a9402801af31","period":30,"genesis_time":1595431050,"hash":"8990e7a9aaed2ffed73dbd7092123d6f289930540d7651336225dc172e51b2ce","groupHash":"176f93498eac9ca337150b46d21dd58673ea4e3581185f869672e59fa4cb390a"}`,
	},
}
