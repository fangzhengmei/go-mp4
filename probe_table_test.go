package mp4

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/sunfish-shogi/bufseekio"
)

func TestProbeSupportsBufferedReadSeeker(t *testing.T) {
	f, err := os.Open("./testdata/sample.mp4")
	require.NoError(t, err)
	defer f.Close()

	info, err := Probe(bufseekio.NewReadSeeker(f, 1024, 4))
	require.NoError(t, err)
	require.Len(t, info.Tracks, 2)
	assert.Len(t, info.Tracks[0].Samples, 10)
	assert.Len(t, info.Tracks[0].Chunks, 9)
}

func TestProbeRejectsInconsistentSampleTables(t *testing.T) {
	original, err := os.ReadFile("./testdata/sample.mp4")
	require.NoError(t, err)

	testCases := []struct {
		name   string
		offset int64
		value  uint32
	}{
		{name: "stts entry count", offset: 7017 + 8 + 4, value: 2},
		{name: "ctts entry count", offset: 7061 + 8 + 4, value: 2},
		{name: "stsc entry count", offset: 7149 + 8 + 4, value: 3},
		{name: "stsz sample count", offset: 7189 + 8 + 8, value: 9},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			data := append([]byte(nil), original...)
			binary.BigEndian.PutUint32(data[tc.offset:], tc.value)
			_, err := Probe(bytes.NewReader(data))
			require.Error(t, err)
		})
	}
}

func TestProbeRejectsTruncatedSampleTable(t *testing.T) {
	original, err := os.ReadFile("./testdata/sample.mp4")
	require.NoError(t, err)

	data := append([]byte(nil), original...)
	binary.BigEndian.PutUint32(data[7189+8+8:], 11)
	_, err = Probe(bytes.NewReader(data))
	require.Error(t, err)
}

func TestProbeRejectsSampleRangeOutsideFile(t *testing.T) {
	original, err := os.ReadFile("./testdata/sample.mp4")
	require.NoError(t, err)

	data := append([]byte(nil), original...)
	binary.BigEndian.PutUint32(data[7249+8+4:], 0)
	binary.BigEndian.PutUint32(data[7189+8+12:], math.MaxUint32)
	_, err = Probe(bytes.NewReader(data))
	require.Error(t, err)
}

func TestProbeRejectsOffsetOverflow(t *testing.T) {
	original, err := os.ReadFile("./testdata/sample.mp4")
	require.NoError(t, err)

	t.Run("stco", func(t *testing.T) {
		data := append([]byte(nil), original...)
		binary.BigEndian.PutUint32(data[7249+8+4:], math.MaxUint32)
		_, err := Probe(bytes.NewReader(data))
		require.Error(t, err)
	})

	t.Run("co64", func(t *testing.T) {
		data := append([]byte(nil), original...)

		stco := data[7249 : 7249+52]
		co64 := make([]byte, 8+4+4+8)
		binary.BigEndian.PutUint32(co64[:4], 8+4+4+8)
		copy(co64[4:], "co64")
		copy(co64[8:], stco[8:12])
		binary.BigEndian.PutUint32(co64[12:], 1)
		binary.BigEndian.PutUint64(co64[16:], math.MaxUint64)

		data = append(data[:7249], append(co64, data[7301:]...)...)
		_, err := Probe(bytes.NewReader(data))
		require.Error(t, err)
	})
}

func TestFindIDRFramesUsesSyncSampleTable(t *testing.T) {
	f, err := os.Open("./testdata/sample.mp4")
	require.NoError(t, err)
	defer f.Close()

	info, err := Probe(f)
	require.NoError(t, err)
	require.NotNil(t, info.Tracks[0].syncSamplesBox)

	_, err = f.Seek(0, 0)
	require.NoError(t, err)
	indexes, err := FindIDRFrames(f, info.Tracks[0])
	require.NoError(t, err)
	assert.Equal(t, []int{0}, indexes)
}

func TestFindIDRFramesRejectsInvalidSyncSampleTable(t *testing.T) {
	original, err := os.ReadFile("./testdata/sample.mp4")
	require.NoError(t, err)

	data := append([]byte(nil), original...)
	binary.BigEndian.PutUint32(data[7041+8+4:], 11)

	info, err := Probe(bytes.NewReader(data))
	require.NoError(t, err)
	_, err = FindIDRFrames(bytes.NewReader(data), info.Tracks[0])
	require.Error(t, err)
}
