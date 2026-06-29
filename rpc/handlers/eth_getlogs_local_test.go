package handlers

import (
	"encoding/hex"
	"strings"
	"testing"

	abi "github.com/filecoin-project/go-state-types/abi"
	"github.com/stretchr/testify/require"

	ltypes "github.com/Reiers/lantern/chain/types"
)

func topicEntry(key string, val []byte) ltypes.EventEntry {
	return ltypes.EventEntry{Codec: rawCodec, Key: key, Value: val}
}

func TestEthLogFromEntries_TopicsAndData(t *testing.T) {
	t1 := make([]byte, 32)
	t1[31] = 0xaa
	t2 := make([]byte, 32)
	t2[31] = 0xbb
	data := []byte{0x01, 0x02, 0x03}

	entries := []ltypes.EventEntry{
		topicEntry("t1", t1),
		topicEntry("t2", t2),
		topicEntry("d", data),
	}
	gotData, topics, ok := ethLogFromEntries(entries)
	require.True(t, ok)
	require.Equal(t, data, gotData)
	require.Len(t, topics, 2)
	require.Equal(t, t1, topics[0])
	require.Equal(t, t2, topics[1])
}

func TestEthLogFromEntries_NonRawCodecDropsEvent(t *testing.T) {
	entries := []ltypes.EventEntry{{Codec: 0x71, Key: "t1", Value: make([]byte, 32)}}
	_, _, ok := ethLogFromEntries(entries)
	require.False(t, ok, "non-raw codec must drop the event")
}

func TestEthLogFromEntries_BadTopicSize(t *testing.T) {
	entries := []ltypes.EventEntry{topicEntry("t1", []byte{0x01})}
	_, _, ok := ethLogFromEntries(entries)
	require.False(t, ok, "mis-sized topic must drop the event")
}

func TestEthLogFromEntries_SkippedTopicIndex(t *testing.T) {
	// t2 present without t1 -> length mismatch -> drop (matches lotus).
	entries := []ltypes.EventEntry{topicEntry("t2", make([]byte, 32))}
	_, _, ok := ethLogFromEntries(entries)
	require.False(t, ok)
}

func TestEthAddrFromActorID(t *testing.T) {
	got := ethAddrFromActorID(abi.ActorID(31))
	// 0xff || 11 zero || be64(31) => last byte 0x1f.
	require.True(t, strings.HasPrefix(got, "0xff"))
	require.Equal(t, "1f", got[len(got)-2:])
}

func TestParseEthLogFilter(t *testing.T) {
	f, ok := parseEthLogFilter(map[string]any{
		"fromBlock": "0x10",
		"toBlock":   "latest",
		"address":   "0xABCdef0000000000000000000000000000000001",
		"topics":    []any{"0xTOPIC0", nil, []any{"0xa", "0xB"}},
	})
	require.True(t, ok)
	require.Equal(t, "0x10", f.fromBlock)
	require.Equal(t, "latest", f.toBlock)
	require.True(t, f.addresses["0xabcdef0000000000000000000000000000000001"])
	require.Len(t, f.topics, 3)
	require.True(t, f.topics[0]["0xtopic0"])
	require.Nil(t, f.topics[1]) // wildcard
	require.True(t, f.topics[2]["0xa"] && f.topics[2]["0xb"])
}

func TestEthLogFilterMatches(t *testing.T) {
	addr := "0x00000000000000000000000000000000000000aa"
	topic := make([]byte, 32)
	topic[31] = 0x42
	topicHex := "0x" + hex.EncodeToString(topic)

	f := ethLogFilter{
		addresses: map[string]bool{addr: true},
		topics:    []map[string]bool{{topicHex: true}},
	}
	require.True(t, f.matches(addr, [][]byte{topic}))
	require.False(t, f.matches("0x00000000000000000000000000000000000000bb", [][]byte{topic}), "wrong address")

	other := make([]byte, 32)
	require.False(t, f.matches(addr, [][]byte{other}), "wrong topic")

	// Wildcard address + wildcard topic position matches anything.
	fAny := ethLogFilter{addresses: map[string]bool{}, topics: []map[string]bool{nil}}
	require.True(t, fAny.matches(addr, [][]byte{topic}))
}

func TestResolveEpochParam(t *testing.T) {
	head := abi.ChainEpoch(1000)
	require.Equal(t, abi.ChainEpoch(1000), resolveEpochParam("latest", head, 0))
	require.Equal(t, abi.ChainEpoch(0), resolveEpochParam("earliest", head, 999))
	require.Equal(t, abi.ChainEpoch(16), resolveEpochParam("0x10", head, 0))
	require.Equal(t, abi.ChainEpoch(42), resolveEpochParam("", head, 42), "empty falls to default")
}

func TestNormalizeHashHex(t *testing.T) {
	h := "0x" + strings.Repeat("Ab", 32)
	require.Equal(t, "0x"+strings.Repeat("ab", 32), normalizeHashHex(h))
	require.Equal(t, "", normalizeHashHex("0x1234"), "wrong length -> empty")
	require.Equal(t, "", normalizeHashHex(""))
}
