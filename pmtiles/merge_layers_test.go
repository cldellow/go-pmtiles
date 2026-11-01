package pmtiles

import (
	"bytes"
	"encoding/binary"
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
	header, md, entries, err := mergeLayerTiles(logger, []io.ReaderAt{bytes.NewReader(a), bytes.NewReader(b)}, []string{"a", "b"}, &tileData)
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
	_, _, entries, err := mergeLayerTiles(logger, []io.ReaderAt{bytes.NewReader(a), bytes.NewReader(b)}, []string{"a", "b"}, &tileData)
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
	a := mvtArchive([]string{"water"}, map[Zxy][]byte{{0, 0, 0}: gzipMvt("water")})
	b := mvtArchive([]string{"water"}, map[Zxy][]byte{{0, 0, 0}: gzipMvt("water")})
	logger := log.New(io.Discard, "", 0)
	_, _, _, err := mergeLayerTiles(logger, []io.ReaderAt{bytes.NewReader(a), bytes.NewReader(b)}, []string{"a", "b"}, io.Discard)
	assert.ErrorContains(t, err, `vector layer "water" is also present`)
}

func TestMergeLayerTilesDuplicateLayerInTile(t *testing.T) {
	// metadata claims disjoint layers, but the tiles disagree
	a := mvtArchive([]string{"water"}, map[Zxy][]byte{{0, 0, 0}: gzipMvt("water")})
	b := mvtArchive([]string{"roads"}, map[Zxy][]byte{{0, 0, 0}: gzipMvt("roads", "water")})
	logger := log.New(io.Discard, "", 0)
	_, _, _, err := mergeLayerTiles(logger, []io.ReaderAt{bytes.NewReader(a), bytes.NewReader(b)}, []string{"a", "b"}, io.Discard)
	assert.ErrorContains(t, err, `layer "water" appears in both a and b at tile 0 0 0`)
}

func TestMergeLayerTilesRequiresMvt(t *testing.T) {
	var h HeaderV3
	h.Clustered = true
	h.TileType = Png
	a := fakeArchive(h, map[string]interface{}{}, map[Zxy][]byte{{0, 0, 0}: {1}}, false, Gzip)
	logger := log.New(io.Discard, "", 0)
	_, _, _, err := mergeLayerTiles(logger, []io.ReaderAt{bytes.NewReader(a), bytes.NewReader(a)}, []string{"a", "b"}, io.Discard)
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
	assert.Nil(t, Merge(logger, []string{pa, pb, out}))

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
	assert.ErrorContains(t, Merge(logger, []string{pa, pa}), "is also an input")
}
