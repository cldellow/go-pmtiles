package pmtiles

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

// fakeMvt returns an uncompressed MVT tile containing empty layers with the given names.
func fakeMvt(layers ...string) []byte {
	var tile []byte
	for _, name := range layers {
		var layer []byte
		layer = binary.AppendUvarint(layer, 15<<3|0) // version
		layer = binary.AppendUvarint(layer, 2)
		layer = binary.AppendUvarint(layer, 1<<3|2) // name
		layer = binary.AppendUvarint(layer, uint64(len(name)))
		layer = append(layer, name...)
		tile = binary.AppendUvarint(tile, 3<<3|2)
		tile = binary.AppendUvarint(tile, uint64(len(layer)))
		tile = append(tile, layer...)
	}
	return tile
}

func gzipMvt(layers ...string) []byte {
	b, _ := compressTile(fakeMvt(layers...), Gzip)
	return b
}

func mvtArchive(layers []string, tiles map[Zxy][]byte) []byte {
	var h HeaderV3
	h.Clustered = true
	h.TileType = Mvt
	var vl []interface{}
	for _, l := range layers {
		vl = append(vl, map[string]interface{}{"id": l})
	}
	return fakeArchive(h, map[string]interface{}{"vector_layers": vl, "name": layers[0]}, tiles, false, Gzip)
}

// readArchiveTiles returns the decompressed layer names for every addressed tile.
func readArchiveTiles(t *testing.T, archive []byte) (HeaderV3, map[string]interface{}, map[uint64][]string) {
	r := bytes.NewReader(archive)
	h, err := DeserializeHeader(archive[:HeaderV3LenBytes])
	assert.Nil(t, err)
	md, err := DeserializeMetadata(io.NewSectionReader(r, int64(h.MetadataOffset), int64(h.MetadataLength)), h.InternalCompression)
	assert.Nil(t, err)
	result := make(map[uint64][]string)
	IterateEntries(h, func(o, l uint64) ([]byte, error) { return archive[o : o+l], nil }, func(e EntryV3) {
		start := h.TileDataOffset + e.Offset
		data, err := decompressTile(archive[start:start+uint64(e.Length)], h.TileCompression)
		assert.Nil(t, err)
		names, err := mvtLayerNames(data)
		assert.Nil(t, err)
		for i := uint64(0); i < uint64(e.RunLength); i++ {
			result[e.TileID+i] = names
		}
	})
	return h, md, result
}

func TestMvtLayerNames(t *testing.T) {
	names, err := mvtLayerNames(fakeMvt("water", "roads"))
	assert.Nil(t, err)
	assert.Equal(t, []string{"water", "roads"}, names)

	_, err = mvtLayerNames([]byte{0x1a, 0x05, 0x01})
	assert.NotNil(t, err)
}

func TestMergeLayerTiles(t *testing.T) {
	a := mvtArchive([]string{"water"}, map[Zxy][]byte{
		{0, 0, 0}: gzipMvt("water"),
		{1, 0, 0}: gzipMvt("water"),
	})
	b := mvtArchive([]string{"roads"}, map[Zxy][]byte{
		{0, 0, 0}: gzipMvt("roads"),
		{1, 1, 1}: gzipMvt("roads"),
	})

	var tileData bytes.Buffer
	logger := log.New(io.Discard, "", 0)
	header, md, entries, err := mergeLayerTiles(logger, []io.ReaderAt{bytes.NewReader(a), bytes.NewReader(b)}, []string{"a", "b"}, UnknownCompression, &tileData)
	assert.Nil(t, err)
	assert.Equal(t, 3, len(entries))
	assert.Equal(t, uint64(3), header.AddressedTilesCount)
	assert.Equal(t, uint8(0), header.MinZoom)
	assert.Equal(t, uint8(1), header.MaxZoom)
	assert.Equal(t, "water", md["name"])
	assert.Equal(t, 2, len(md["vector_layers"].([]interface{})))

	e := entries[0]
	data, err := decompressTile(tileData.Bytes()[e.Offset:e.Offset+uint64(e.Length)], Gzip)
	assert.Nil(t, err)
	names, _ := mvtLayerNames(data)
	assert.Equal(t, []string{"water", "roads"}, names)
}

func TestMergeLayerTilesSplitsRuns(t *testing.T) {
	// archive a has one tile repeated over all of z1 (a run of 4); b has a single z1 tile.
	ocean := gzipMvt("water")
	a := mvtArchive([]string{"water"}, map[Zxy][]byte{
		{1, 0, 0}: ocean, {1, 0, 1}: ocean, {1, 1, 1}: ocean, {1, 1, 0}: ocean,
	})
	b := mvtArchive([]string{"roads"}, map[Zxy][]byte{
		{1, 1, 1}: gzipMvt("roads"),
	})
	// make a deduplicated version of a, so its tiles form a single run-length entry
	var h HeaderV3
	h.Clustered = true
	h.TileType = Mvt
	_ = a
	resolver := newResolver(true, false)
	resolver.AddTileIsNew(ZxyToID(1, 0, 0), ocean, 4)
	assert.Equal(t, 1, len(resolver.Entries))
	rootBytes := SerializeEntries(resolver.Entries, Gzip)
	md, _ := SerializeMetadata(map[string]interface{}{"vector_layers": []interface{}{map[string]interface{}{"id": "water"}}}, Gzip)
	h.InternalCompression = Gzip
	h.TileCompression = Gzip
	h.RootOffset = HeaderV3LenBytes
	h.RootLength = uint64(len(rootBytes))
	h.MetadataOffset = h.RootOffset + h.RootLength
	h.MetadataLength = uint64(len(md))
	h.LeafDirectoryOffset = h.MetadataOffset + h.MetadataLength
	h.TileDataOffset = h.LeafDirectoryOffset
	h.TileDataLength = uint64(len(ocean))
	a = append(SerializeHeader(h), rootBytes...)
	a = append(a, md...)
	a = append(a, ocean...)

	var tileData bytes.Buffer
	logger := log.New(io.Discard, "", 0)
	_, _, entries, err := mergeLayerTiles(logger, []io.ReaderAt{bytes.NewReader(a), bytes.NewReader(b)}, []string{"a", "b"}, UnknownCompression, &tileData)
	assert.Nil(t, err)

	id := ZxyToID(1, 1, 1)
	// the run is split around the overlapping tile: [1..id) ocean, id merged, (id..4] ocean
	assert.Equal(t, 3, len(entries))
	assert.Equal(t, entries[0].Offset, entries[2].Offset, "ocean tile is deduplicated")
	assert.Equal(t, id, entries[1].TileID)
	assert.Equal(t, uint32(1), entries[1].RunLength)
	assert.Equal(t, uint32(4), entries[0].RunLength+entries[1].RunLength+entries[2].RunLength)
}

func TestMergeLayerTilesDuplicateLayerInMetadata(t *testing.T) {
	// Both inputs declare "parks", but never put it in the same tile.
	a := mvtArchive([]string{"parks", "pois"}, map[Zxy][]byte{
		{0, 0, 0}: gzipMvt("pois"),
		{1, 0, 0}: gzipMvt("parks"),
	})
	b := mvtArchive([]string{"parks"}, map[Zxy][]byte{
		{0, 0, 0}: gzipMvt("parks"),
	})
	logger := log.New(io.Discard, "", 0)
	_, md, entries, err := mergeLayerTiles(logger, []io.ReaderAt{bytes.NewReader(a), bytes.NewReader(b)}, []string{"a", "b"}, UnknownCompression, io.Discard)
	assert.Nil(t, err)
	assert.Equal(t, 2, len(entries))
	var ids []string
	for _, l := range md["vector_layers"].([]interface{}) {
		ids = append(ids, l.(map[string]interface{})["id"].(string))
	}
	assert.Equal(t, []string{"parks", "pois"}, ids, "each layer is listed once")
}

func TestDedupeLayers(t *testing.T) {
	layers := []interface{}{
		map[string]interface{}{"id": "parks", "minzoom": 8.0, "maxzoom": 12.0, "fields": map[string]interface{}{"name": "String"}},
		map[string]interface{}{"id": "pois"},
		map[string]interface{}{"id": "parks", "minzoom": 6.0, "maxzoom": 14.0, "fields": map[string]interface{}{"name": "Number", "area": "Number"}},
	}
	result := dedupeLayers(layers, "id")
	assert.Equal(t, 2, len(result))
	parks := result[0].(map[string]interface{})
	assert.Equal(t, 6.0, parks["minzoom"])
	assert.Equal(t, 14.0, parks["maxzoom"])
	assert.Equal(t, map[string]interface{}{"name": "String", "area": "Number"}, parks["fields"])
	// the inputs are left alone
	assert.Equal(t, 8.0, layers[0].(map[string]interface{})["minzoom"])
	assert.Equal(t, 1, len(layers[0].(map[string]interface{})["fields"].(map[string]interface{})))
}

func TestMergeLayerTilesDropsEmptyTiles(t *testing.T) {
	empty, _ := compressTile(nil, Gzip)
	a := mvtArchive([]string{"water"}, map[Zxy][]byte{
		{0, 0, 0}: empty,
		{1, 0, 0}: empty,
		{1, 0, 1}: gzipMvt("water"),
	})
	b := mvtArchive([]string{"roads"}, map[Zxy][]byte{
		{1, 0, 0}: empty,
		{1, 1, 1}: gzipMvt("roads"),
	})
	logger := log.New(io.Discard, "", 0)
	header, _, entries, err := mergeLayerTiles(logger, []io.ReaderAt{bytes.NewReader(a), bytes.NewReader(b)}, []string{"a", "b"}, UnknownCompression, io.Discard)
	assert.Nil(t, err)
	assert.Equal(t, uint64(2), header.AddressedTilesCount)
	assert.Equal(t, 2, len(entries))
	assert.Equal(t, ZxyToID(1, 0, 1), entries[0].TileID)
	assert.Equal(t, uint8(1), header.MinZoom)
}

// manyTileArchives makes three inputs with overlapping, partly repeated tiles at z0-6.
func manyTileArchives() [][]byte {
	layers := []string{"water", "roads", "pois"}
	var archives [][]byte
	for i, layer := range layers {
		tiles := make(map[Zxy][]byte)
		repeated := gzipMvt(layer)
		for z := uint8(0); z <= 6; z++ {
			for x := uint32(0); x < 1<<z; x++ {
				for y := uint32(0); y < 1<<z; y++ {
					switch (x*7 + y*3 + uint32(i)) % 5 {
					case 0:
						// absent
					case 1:
						tiles[Zxy{z, x, y}] = repeated
					default:
						tiles[Zxy{z, x, y}] = gzipMvt(layer, fmt.Sprintf("%s_%d_%d_%d", layer, z, x, y))
					}
				}
			}
		}
		archives = append(archives, mvtArchive([]string{layer}, tiles))
	}
	return archives
}

func TestMergeSegmentsParallelMatchesSerial(t *testing.T) {
	archives := manyTileArchives()
	run := func(workers int) ([]EntryV3, []byte) {
		var readers []io.ReaderAt
		for _, a := range archives {
			readers = append(readers, bytes.NewReader(a))
		}
		inputs, err, _ := loadLayerMergeInputs(readers)
		assert.Nil(t, err)
		var tileData bytes.Buffer
		entries, _, _, err := mergeSegments(inputs, []string{"a", "b", "c"}, Gzip, workers, &tileData)
		assert.Nil(t, err)
		return entries, tileData.Bytes()
	}
	serialEntries, serialData := run(1)
	assert.Greater(t, len(serialEntries), 1000)
	for _, workers := range []int{2, 8, 32} {
		entries, data := run(workers)
		assert.Equal(t, serialEntries, entries, "workers=%d", workers)
		assert.True(t, bytes.Equal(serialData, data), "workers=%d", workers)
	}
}

func TestMergeSegmentsErrorDoesNotHang(t *testing.T) {
	archives := manyTileArchives()
	// a fourth input that clashes with "water" late in the archive
	archives = append(archives, mvtArchive([]string{"water2"}, map[Zxy][]byte{
		{6, 63, 62}: gzipMvt("water"), // present in "water" too: (63*7+62*3)%5 != 0
	}))
	var readers []io.ReaderAt
	for _, a := range archives {
		readers = append(readers, bytes.NewReader(a))
	}
	inputs, err, _ := loadLayerMergeInputs(readers)
	assert.Nil(t, err)
	_, _, _, err = mergeSegments(inputs, []string{"a", "b", "c", "d"}, Gzip, 4, io.Discard)
	assert.ErrorContains(t, err, `layer "water" appears in both a and d`)
}

func TestMergeLayerTilesDuplicateLayerInTile(t *testing.T) {
	// metadata claims disjoint layers, but the tiles disagree
	a := mvtArchive([]string{"water"}, map[Zxy][]byte{{0, 0, 0}: gzipMvt("water")})
	b := mvtArchive([]string{"roads"}, map[Zxy][]byte{{0, 0, 0}: gzipMvt("roads", "water")})
	logger := log.New(io.Discard, "", 0)
	_, _, _, err := mergeLayerTiles(logger, []io.ReaderAt{bytes.NewReader(a), bytes.NewReader(b)}, []string{"a", "b"}, UnknownCompression, io.Discard)
	assert.ErrorContains(t, err, `layer "water" appears in both a and b at tile 0 0 0`)
}

func TestMergeLayerTilesRequiresMvt(t *testing.T) {
	var h HeaderV3
	h.Clustered = true
	h.TileType = Png
	a := fakeArchive(h, map[string]interface{}{}, map[Zxy][]byte{{0, 0, 0}: {1}}, false, Gzip)
	logger := log.New(io.Discard, "", 0)
	_, _, _, err := mergeLayerTiles(logger, []io.ReaderAt{bytes.NewReader(a), bytes.NewReader(a)}, []string{"a", "b"}, UnknownCompression, io.Discard)
	assert.ErrorContains(t, err, "requires mvt")
}

func TestMergeLayerMetadataTilestats(t *testing.T) {
	md := mergeLayerMetadata([]map[string]interface{}{
		{"tilestats": map[string]interface{}{"layerCount": 1, "layers": []interface{}{"a"}}},
		{"tilestats": map[string]interface{}{"layerCount": 1, "layers": []interface{}{"b"}}},
	})
	assert.Equal(t, 2, md["tilestats"].(map[string]interface{})["layerCount"])

	md = mergeLayerMetadata([]map[string]interface{}{
		{"tilestats": map[string]interface{}{"layerCount": 1, "layers": []interface{}{"a"}}},
		{},
	})
	_, ok := md["tilestats"]
	assert.False(t, ok)
}

func TestMergeEndToEndOverlappingMvt(t *testing.T) {
	dir := t.TempDir()
	a := mvtArchive([]string{"water"}, map[Zxy][]byte{
		{0, 0, 0}: gzipMvt("water"),
		{1, 0, 0}: gzipMvt("water"),
	})
	b := mvtArchive([]string{"roads"}, map[Zxy][]byte{
		{0, 0, 0}: gzipMvt("roads"),
		{2, 3, 3}: gzipMvt("roads"),
	})
	pa, pb, out := filepath.Join(dir, "a.pmtiles"), filepath.Join(dir, "b.pmtiles"), filepath.Join(dir, "out.pmtiles")
	assert.Nil(t, os.WriteFile(pa, a, 0644))
	assert.Nil(t, os.WriteFile(pb, b, 0644))

	logger := log.New(io.Discard, "", 0)
	assert.Nil(t, Merge(logger, []string{pa, pb, out}, ""))

	result, err := os.ReadFile(out)
	assert.Nil(t, err)
	h, md, tiles := readArchiveTiles(t, result)
	assert.Equal(t, TileType(Mvt), h.TileType)
	assert.Equal(t, uint8(2), h.MaxZoom)
	assert.Equal(t, 2, len(md["vector_layers"].([]interface{})))
	assert.Equal(t, map[uint64][]string{
		ZxyToID(0, 0, 0): {"water", "roads"},
		ZxyToID(1, 0, 0): {"water"},
		ZxyToID(2, 3, 3): {"roads"},
	}, tiles)

	// no temp files left behind
	files, _ := os.ReadDir(dir)
	assert.Equal(t, 3, len(files))
}

func TestMergeRejectsOutputAsInput(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	dir := t.TempDir()
	a := mvtArchive([]string{"water"}, map[Zxy][]byte{{0, 0, 0}: gzipMvt("water")})
	pa := filepath.Join(dir, "a.pmtiles")
	assert.Nil(t, os.WriteFile(pa, a, 0644))
	assert.ErrorContains(t, Merge(logger, []string{pa, pa}, ""), "is also an input")
}

func zstdMvt(layers ...string) []byte {
	b, _ := compressTile(fakeMvt(layers...), Zstd)
	return b
}

// withTileCompression returns a copy of archive whose header claims the given tile compression.
func withTileCompression(archive []byte, c Compression) []byte {
	out := append([]byte(nil), archive...)
	h, _ := DeserializeHeader(out[:HeaderV3LenBytes])
	h.TileCompression = c
	copy(out, SerializeHeader(h))
	return out
}

func TestCompressTileRoundTrip(t *testing.T) {
	data := fakeMvt("water", "roads", "places")
	for _, c := range []Compression{NoCompression, Gzip, Zstd} {
		b, err := compressTile(data, c)
		assert.Nil(t, err)
		out, err := decompressTile(b, c)
		assert.Nil(t, err)
		assert.Equal(t, data, out)
	}
	b, _ := compressTile(data, Zstd)
	assert.Equal(t, []byte{0x28, 0xb5, 0x2f, 0xfd}, b[:4])

	// an empty tile still makes a frame, which decodes to nothing
	b, _ = compressTile(nil, Zstd)
	assert.NotEmpty(t, b)
	out, err := decompressTile(b, Zstd)
	assert.Nil(t, err)
	assert.Empty(t, out)

	_, err = decompressTile([]byte("not zstd"), Zstd)
	assert.NotNil(t, err)
}

func writeArchives(t *testing.T, archives ...[]byte) (paths []string, out string) {
	dir := t.TempDir()
	for i, a := range archives {
		p := filepath.Join(dir, fmt.Sprintf("in%d.pmtiles", i))
		assert.Nil(t, os.WriteFile(p, a, 0644))
		paths = append(paths, p)
	}
	return paths, filepath.Join(dir, "out.pmtiles")
}

func TestMergeZstd(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	zstdA := withTileCompression(mvtArchive([]string{"water"}, map[Zxy][]byte{
		{0, 0, 0}: zstdMvt("water"),
		{1, 0, 0}: zstdMvt("water"),
		{1, 1, 0}: zstdMvt(), // empty
	}), Zstd)
	zstdB := withTileCompression(mvtArchive([]string{"roads"}, map[Zxy][]byte{
		{0, 0, 0}: zstdMvt("roads"),
		{2, 3, 3}: zstdMvt("roads"),
	}), Zstd)
	gzipB := mvtArchive([]string{"roads"}, map[Zxy][]byte{
		{0, 0, 0}: gzipMvt("roads"),
		{2, 3, 3}: gzipMvt("roads"),
	})
	want := map[uint64][]string{
		ZxyToID(0, 0, 0): {"water", "roads"},
		ZxyToID(1, 0, 0): {"water"},
		ZxyToID(2, 3, 3): {"roads"},
	}

	cases := []struct {
		name     string
		inputs   [][]byte
		flag     string
		expected Compression
	}{
		{"zstd inputs stay zstd", [][]byte{zstdA, zstdB}, "", Zstd},
		{"mixed inputs default to gzip", [][]byte{zstdA, gzipB}, "", Gzip},
		{"mixed inputs to zstd", [][]byte{zstdA, gzipB}, "zstd", Zstd},
		{"zstd inputs to gzip", [][]byte{zstdA, zstdB}, "gzip", Gzip},
		{"zstd inputs to none", [][]byte{zstdA, zstdB}, "none", NoCompression},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			paths, out := writeArchives(t, tc.inputs...)
			assert.Nil(t, Merge(logger, append(paths, out), tc.flag))
			result, err := os.ReadFile(out)
			assert.Nil(t, err)
			h, _, tiles := readArchiveTiles(t, result)
			assert.Equal(t, tc.expected, h.TileCompression)
			assert.Equal(t, want, tiles)
		})
	}
}

func TestMergeRecompressesDisjointArchives(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	// disjoint inputs would otherwise be concatenated without decompressing
	a := mvtArchive([]string{"water"}, map[Zxy][]byte{{0, 0, 0}: gzipMvt("water")})
	b := mvtArchive([]string{"water"}, map[Zxy][]byte{{1, 0, 0}: gzipMvt("water")})
	paths, out := writeArchives(t, a, b)
	assert.Nil(t, Merge(logger, append(paths, out), "zstd"))
	result, err := os.ReadFile(out)
	assert.Nil(t, err)
	h, _, tiles := readArchiveTiles(t, result)
	assert.Equal(t, Compression(Zstd), h.TileCompression)
	assert.Equal(t, map[uint64][]string{
		ZxyToID(0, 0, 0): {"water"},
		ZxyToID(1, 0, 0): {"water"},
	}, tiles)

	assert.ErrorContains(t, Merge(logger, append(paths, out), "brotli"), "unsupported tile compression")
}

func TestMergeLayerTilesZstdInternalCompression(t *testing.T) {
	var h HeaderV3
	h.Clustered = true
	h.TileType = Mvt
	a := fakeArchive(h, map[string]interface{}{"vector_layers": []interface{}{map[string]interface{}{"id": "water"}}},
		map[Zxy][]byte{{0, 0, 0}: gzipMvt("water")}, true, Zstd)
	hdr, md, tiles := readArchiveTiles(t, a)
	assert.Equal(t, Compression(Zstd), hdr.InternalCompression)
	assert.Equal(t, "water", md["vector_layers"].([]interface{})[0].(map[string]interface{})["id"])
	assert.Equal(t, map[uint64][]string{ZxyToID(0, 0, 0): {"water"}}, tiles)

	b := mvtArchive([]string{"roads"}, map[Zxy][]byte{{0, 0, 0}: gzipMvt("roads")})
	logger := log.New(io.Discard, "", 0)
	_, md, _, err := mergeLayerTiles(logger, []io.ReaderAt{bytes.NewReader(a), bytes.NewReader(b)}, []string{"a", "b"}, UnknownCompression, io.Discard)
	assert.Nil(t, err)
	assert.Equal(t, 2, len(md["vector_layers"].([]interface{})))
}

// The layer merge writes the tile data straight after a reserved 16 KiB, and
// the metadata and leaf directories after the tile data.
func TestMergeLayersLayout(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	// Verify wants real bounds; the test archives have none.
	withBounds := func(archive []byte) []byte {
		out := append([]byte(nil), archive...)
		h, _ := DeserializeHeader(out[:HeaderV3LenBytes])
		h.MinLonE7, h.MinLatE7, h.MaxLonE7, h.MaxLatE7 = -10e7, -10e7, 10e7, 10e7
		copy(out, SerializeHeader(h))
		return out
	}
	a := mvtArchive([]string{"water"}, map[Zxy][]byte{
		{0, 0, 0}: gzipMvt("water"),
		{1, 0, 0}: gzipMvt("water"),
	})
	b := mvtArchive([]string{"roads"}, map[Zxy][]byte{
		{0, 0, 0}: gzipMvt("roads"),
		{2, 3, 3}: gzipMvt("roads"),
	})
	paths, out := writeArchives(t, withBounds(a), withBounds(b))
	assert.Nil(t, Merge(logger, append(paths, out), ""))
	assert.Nil(t, Verify(logger, out))

	result, err := os.ReadFile(out)
	assert.Nil(t, err)
	h, _, tiles := readArchiveTiles(t, result)
	assert.Equal(t, uint64(HeaderV3LenBytes), h.RootOffset)
	assert.Equal(t, uint64(16384), h.TileDataOffset)
	assert.Equal(t, h.TileDataOffset+h.TileDataLength, h.MetadataOffset)
	assert.Equal(t, h.MetadataOffset+h.MetadataLength, h.LeafDirectoryOffset)
	assert.Equal(t, int(h.LeafDirectoryOffset+h.LeafDirectoryLength), len(result))
	assert.Equal(t, map[uint64][]string{
		ZxyToID(0, 0, 0): {"water", "roads"},
		ZxyToID(1, 0, 0): {"water"},
		ZxyToID(2, 3, 3): {"roads"},
	}, tiles)

	// A failed merge leaves no output behind.
	bad := filepath.Join(t.TempDir(), "bad.pmtiles")
	assert.Nil(t, os.WriteFile(bad, []byte("not a pmtiles archive"), 0644))
	failed := filepath.Join(t.TempDir(), "failed.pmtiles")
	assert.NotNil(t, Merge(logger, []string{paths[0], bad, failed}, ""))
	_, err = os.Stat(failed)
	assert.True(t, os.IsNotExist(err))
}
