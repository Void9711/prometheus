package tsdb

import (
	"encoding/binary"
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"testing"

	"github.com/coreos/etcd/pkg/fileutil"
	"github.com/stretchr/testify/require"
)

func TestWAL_initSegments(t *testing.T) {
	tmpdir, err := ioutil.TempDir("", "test_wal_open")
	require.NoError(t, err)
	defer os.RemoveAll(tmpdir)

	df, err := fileutil.OpenDir(tmpdir)
	require.NoError(t, err)

	w := &WAL{dirFile: df}

	// Create segment files with an appropriate header.
	for i := 1; i <= 5; i++ {
		metab := make([]byte, 8)
		binary.BigEndian.PutUint32(metab[:4], WALMagic)
		metab[4] = WALFormatDefault

		f, err := os.Create(fmt.Sprintf("%s/000%d", tmpdir, i))
		require.NoError(t, err)
		_, err = f.Write(metab)
		require.NoError(t, err)
		require.NoError(t, f.Close())
	}

	// Initialize 5 correct segment files.
	require.NoError(t, w.initSegments())

	require.Equal(t, 5, len(w.files), "unexpected number of segments loaded")

	// Validate that files are locked properly.
	for _, of := range w.files {
		f, err := os.Open(of.Name())
		require.NoError(t, err, "open locked segment %s", f.Name())

		_, err = f.Read([]byte{0})
		require.NoError(t, err, "read locked segment %s", f.Name())

		_, err = f.Write([]byte{0})
		require.Error(t, err, "write to tail segment file %s", f.Name())

		require.NoError(t, f.Close())
	}

	for _, f := range w.files {
		require.NoError(t, f.Close())
	}

	// Make initialization fail by corrupting the header of one file.
	f, err := os.OpenFile(w.files[3].Name(), os.O_WRONLY, 0666)
	require.NoError(t, err)

	_, err = f.WriteAt([]byte{0}, 4)
	require.NoError(t, err)

	w = &WAL{dirFile: df}
	require.Error(t, w.initSegments(), "init corrupted segments")

	for _, f := range w.files {
		require.NoError(t, f.Close())
	}
}

func TestWAL_cut(t *testing.T) {
	tmpdir, err := ioutil.TempDir("", "test_wal_cut")
	require.NoError(t, err)
	defer os.RemoveAll(tmpdir)

	// This calls cut() implicitly the first time without a previous tail.
	w, err := OpenWAL(tmpdir, nil, 0)
	require.NoError(t, err)

	require.NoError(t, w.entry(WALEntrySeries, 1, []byte("Hello World!!")))

	require.NoError(t, w.cut(), "cut failed")

	// Cutting creates a new file and close the previous tail file.
	require.Equal(t, 2, len(w.files))
	require.Equal(t, os.ErrInvalid.Error(), w.files[0].Close().Error())

	require.NoError(t, w.entry(WALEntrySeries, 1, []byte("Hello World!!")))

	require.NoError(t, w.Close())

	for _, of := range w.files {
		f, err := os.Open(of.Name())
		require.NoError(t, err)

		// Verify header data.
		metab := make([]byte, 8)
		_, err = f.Read(metab)
		require.NoError(t, err, "read meta data %s", f.Name())
		require.Equal(t, WALMagic, binary.BigEndian.Uint32(metab[:4]), "verify magic")
		require.Equal(t, WALFormatDefault, metab[4], "verify format flag")

		// We cannot actually check for correct pre-allocation as it is
		// optional per filesystem and handled transparently.
		et, flag, b, err := NewWALReader(f).nextEntry()
		require.NoError(t, err)
		require.Equal(t, WALEntrySeries, et)
		require.Equal(t, flag, byte(walSeriesSimple))
		require.Equal(t, []byte("Hello World!!"), b)
	}
}

// Symmetrical test of reading and writing to the WAL via its main interface.
func TestWAL_Log_Restore(t *testing.T) {
	// Generate testing data. It does not make semantical sense but
	// for the purpose of this test.
	series, err := readPrometheusLabels("testdata/20k.series", 10000)
	require.NoError(t, err)

	var samples []refdSample
	for i := 0; i < 200000; i++ {
		samples = append(samples, refdSample{
			ref: uint64(i % 10000),
			t:   int64(i * 2),
			v:   rand.Float64(),
		})
	}

	dir, err := ioutil.TempDir("", "test_wal_log_restore")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	w, err := OpenWAL(dir, nil, 0)
	require.NoError(t, err)

	// Set smaller segment size so we can actually write several files.
	w.segmentSize = 300 * 1000

	for i := 0; i < len(series); i += 100 {
		require.NoError(t, w.Log(series[i:i+100], samples[i*10:(i+100)*10]))
	}

	require.NoError(t, w.Close())

	w, err = OpenWAL(dir, nil, 0)
	r := w.Reader()

	var i, j int

	for r.Next() {
		lsets, smpls := r.At()

		if l := len(lsets); l > 0 {
			require.Equal(t, series[i:i+l], lsets)
			i += l
		}
		if l := len(smpls); l > 0 {
			require.Equal(t, samples[j:j+l], smpls)
			j += l
		}
	}
	require.NoError(t, r.Err())
}
