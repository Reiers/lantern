package actors

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/ipfs/go-cid"

	evm18 "github.com/filecoin-project/go-state-types/builtin/v18/evm"
)

// Golden fixture: the on-chain EVM actor head block for the calibration
// ServiceProviderRegistry (0x839e5c9988e4e9977d40708d0094103c0839Ac9D ->
// t410fqopfzgmi4tuzo7kaocgqbfaqhqedtle5tied7ii), captured 2026-06-15 via
// Filecoin.ChainReadObj on the Head CID
// bafy2bzacecotea5anb46fwhsp7sqb7jzqxl7r7heht2q3ycwxfxldc2uqyduk.
//
// Glif StateReadState on the same actor reported:
//
//	Bytecode     = bafk2bzaceareebbbmocjw7gkhb4b3axntwta55qrvcevylsbwz33nd6ek3q7g
//	BytecodeHash = 0x88ddba095415fc48347bbd3218062b9f83ca3fcd57e075d72f91df24f43683ef
//
// and eth_getCode keccak256 == that same BytecodeHash, so this fixture is
// the real chain truth for the registry contract.
const (
	regHeadHex = "86d82a5827000155a0e402202242042163849b7cca38781d82ed9da60ef611a8895c2e41b677b68fc456e1f3582088ddba095415fc48347bbd3218062b9f83ca3fcd57e075d72f91df24f43683efd82a5827000171a0e40220c67c4a833dd9d03299dd783fd98c34c5d0c80272d119611f10806abcd4510ff4f601f6"

	wantBytecodeCID  = "bafk2bzaceareebbbmocjw7gkhb4b3axntwta55qrvcevylsbwz33nd6ek3q7g"
	wantBytecodeHash = "88ddba095415fc48347bbd3218062b9f83ca3fcd57e075d72f91df24f43683ef"
	// ContractState (storage KAMT root) read off the same StateReadState.
	wantStorageRoot = "bafy2bzaceddhysudhxm5amuz3v4d7wmmgtc5bsacolirsyi7ccagvpgukeh7i"
)

// TestEVMStateDecode_RegistryGolden decodes the captured registry EVM head
// block directly through the go-state-types v18 evm.State decoder and
// verifies the Bytecode CID + BytecodeHash match the values Glif reported
// for the live contract. This is the Stage-1 (lantern#43 Part B) verify
// gate: it proves we can recover bytecode + storage-root handles from raw,
// CID-verified chain state with no upstream RPC.
func TestEVMStateDecode_RegistryGolden(t *testing.T) {
	raw, err := hex.DecodeString(regHeadHex)
	if err != nil {
		t.Fatalf("decode fixture hex: %v", err)
	}

	var s evm18.State
	if err := s.UnmarshalCBOR(bytes.NewReader(raw)); err != nil {
		t.Fatalf("UnmarshalCBOR evm18.State: %v", err)
	}

	if got := s.Bytecode.String(); got != wantBytecodeCID {
		t.Errorf("Bytecode CID: got %s, want %s", got, wantBytecodeCID)
	}
	if got := hex.EncodeToString(s.BytecodeHash[:]); got != wantBytecodeHash {
		t.Errorf("BytecodeHash: got %s, want %s", got, wantBytecodeHash)
	}
	if got := s.ContractState.String(); got != wantStorageRoot {
		t.Errorf("ContractState (storage root): got %s, want %s", got, wantStorageRoot)
	}
}

// TestLoadEVM_RegistryGolden exercises the full LoadEVM path (registry
// version dispatch + CID-verified head fetch + decode) against a
// single-block BlockGetter holding the captured registry head, then
// confirms the exposed handles match the golden values.
func TestLoadEVM_RegistryGolden(t *testing.T) {
	raw, err := hex.DecodeString(regHeadHex)
	if err != nil {
		t.Fatalf("decode fixture hex: %v", err)
	}

	// The actor's code CID. We register it as KindEvm v18 in a throwaway
	// registry so version dispatch resolves without needing the full
	// bundle wiring.
	codeCid := mustParseCID(t, "bafk2bzaceb63lj5qyx6hrtagl4mlqla6rb2ligpeglurhfvn2avubim62oyxc")
	headCid := mustParseCID(t, "bafy2bzacecotea5anb46fwhsp7sqb7jzqxl7r7heht2q3ycwxfxldc2uqyduk")

	reg := &Registry{byCode: map[cid.Cid]CodeInfo{
		codeCid: {Kind: KindEvm, Version: 18},
	}}

	bg := &mapBlockGetter{blocks: map[cid.Cid][]byte{headCid: raw}}

	st, err := LoadEVM(context.Background(), codeCid, headCid, bg, reg)
	if err != nil {
		t.Fatalf("LoadEVM: %v", err)
	}
	if st.Version() != 18 {
		t.Errorf("Version: got %d, want 18", st.Version())
	}
	if got := st.BytecodeCID().String(); got != wantBytecodeCID {
		t.Errorf("BytecodeCID: got %s, want %s", got, wantBytecodeCID)
	}
	if got := hex.EncodeToString(func() []byte { h := st.BytecodeHash(); return h[:] }()); got != wantBytecodeHash {
		t.Errorf("BytecodeHash: got %s, want %s", got, wantBytecodeHash)
	}
	if got := st.StorageRoot().String(); got != wantStorageRoot {
		t.Errorf("StorageRoot: got %s, want %s", got, wantStorageRoot)
	}
}

// ----- test helpers -----

func mustParseCID(t *testing.T, s string) cid.Cid {
	t.Helper()
	c, err := cid.Parse(s)
	if err != nil {
		t.Fatalf("parse CID %q: %v", s, err)
	}
	return c
}

// mapBlockGetter is a hamt.BlockGetter backed by an in-memory map, for
// tests. CIDs are not re-derived here; VerifyBlockCID inside LoadEVM does
// the integrity check against the requested CID.
type mapBlockGetter struct{ blocks map[cid.Cid][]byte }

func (m *mapBlockGetter) Get(ctx context.Context, c cid.Cid) ([]byte, error) {
	b, ok := m.blocks[c]
	if !ok {
		return nil, fmt.Errorf("mapBlockGetter: missing %s", c)
	}
	return b, nil
}
