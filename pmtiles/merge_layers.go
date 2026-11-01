package pmtiles

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Merging of MVT archives whose tiles overlap but whose layers are disjoint
// (case 3 in https://github.com/protomaps/go-pmtiles/issues/105).
//
// An MVT Tile message is `repeated Layer layers = 3`, so concatenating two
// decompressed tiles yields a valid tile containing the union of their layers.

// overlapError is returned by prepareInputs when inputs address the same tiles.
type overlapError struct {
	count uint64
	z     uint8
	x, y  uint32
}

func (e *overlapError) Error() string {
	return fmt.Sprintf("%d overlapping tiles, starting with %d %d %d. Inputs must be disjoint", e.count, e.z, e.x, e.y)
}

type layerMergeInput struct {
	reader   io.ReaderAt
	header   HeaderV3
	metadata map[string]interface{}
	entries  []EntryV3
	pos      int // cursor into entries
}

// a contiguous range of tile IDs [start, end) over which every input's covering entry is constant.
type layerSegment struct {
	start, end uint64
	sources    []int // indices of inputs covering this segment, ascending
}

// mvtLayerNames returns the names of the layers in a decompressed MVT tile.
func mvtLayerNames(data []byte) ([]string, error) {
	var names []string
	err := walkProtobuf(data, func(field uint64, value []byte) error {
		if field != 3 {
			return nil
		}
		found := false
		err := walkProtobuf(value, func(f uint64, v []byte) error {
			if f == 1 {
				names = append(names, string(v))
				found = true
			}
			return nil
		})
		if err != nil {
			return err
		}
		if !found {
			return errors.New("layer without a name")
		}
		return nil
	})
	return names, err
}

// walkProtobuf calls fn for each length-delimited field in msg, skipping other wire types.
func walkProtobuf(msg []byte, fn func(field uint64, value []byte) error) error {
	for len(msg) > 0 {
		tag, n := binary.Uvarint(msg)
		if n <= 0 {
			return errors.New("invalid protobuf tag")
		}
		msg = msg[n:]
		field, wireType := tag>>3, tag&7
		switch wireType {
		case 0:
			_, n := binary.Uvarint(msg)
			if n <= 0 {
				return errors.New("invalid protobuf varint")
			}
			msg = msg[n:]
		case 1:
			if len(msg) < 8 {
				return errors.New("truncated protobuf fixed64")
			}
			msg = msg[8:]
		case 5:
			if len(msg) < 4 {
				return errors.New("truncated protobuf fixed32")
			}
			msg = msg[4:]
		case 2:
			l, n := binary.Uvarint(msg)
			if n <= 0 || l > uint64(len(msg)-n) {
				return errors.New("invalid protobuf length")
			}
			value := msg[n : n+int(l)]
			msg = msg[n+int(l):]
			if err := fn(field, value); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported protobuf wire type %d", wireType)
		}
	}
	return nil
}

func decompressTile(data []byte, compression Compression) ([]byte, error) {
	switch compression {
	case NoCompression:
		return data, nil
	case Gzip:
		r, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		defer r.Close()
		return io.ReadAll(r)
	default:
		c, _ := compressionToString(compression)
		return nil, fmt.Errorf("tile compression %s not supported for layer merging", c)
	}
}

func compressTile(data []byte, compression Compression) ([]byte, error) {
	switch compression {
	case NoCompression:
		return data, nil
	case Gzip:
		var b bytes.Buffer
		w, err := gzip.NewWriterLevel(&b, gzip.BestCompression)
		if err != nil {
			return nil, err
		}
		w.Write(data)
		if err := w.Close(); err != nil {
			return nil, err
		}
		return b.Bytes(), nil
	default:
		c, _ := compressionToString(compression)
		return nil, fmt.Errorf("tile compression %s not supported for layer merging", c)
	}
}

func vectorLayerIDs(metadata map[string]interface{}) ([]string, bool) {
	layers, ok := metadata["vector_layers"].([]interface{})
	if !ok {
		return nil, false
	}
	var ids []string
	for _, l := range layers {
		if m, ok := l.(map[string]interface{}); ok {
			if id, ok := m["id"].(string); ok {
				ids = append(ids, id)
			}
		}
	}
	return ids, true
}

// mergeLayerMetadata copies the first input's metadata, concatenating vector_layers
// and tilestats.layers across all inputs.
func mergeLayerMetadata(metadatas []map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{})
	for k, v := range metadatas[0] {
		result[k] = v
	}

	var vectorLayers []interface{}
	for _, m := range metadatas {
		if l, ok := m["vector_layers"].([]interface{}); ok {
			vectorLayers = append(vectorLayers, l...)
		}
	}
	if vectorLayers != nil {
		result["vector_layers"] = vectorLayers
	}

	var statLayers []interface{}
	allHaveStats := true
	for _, m := range metadatas {
		ts, ok := m["tilestats"].(map[string]interface{})
		if !ok {
			allHaveStats = false
			break
		}
		l, ok := ts["layers"].([]interface{})
		if !ok {
			allHaveStats = false
			break
		}
		statLayers = append(statLayers, l...)
	}
	if allHaveStats {
		result["tilestats"] = map[string]interface{}{
			"layerCount": len(statLayers),
			"layers":     statLayers,
		}
	} else {
		delete(result, "tilestats")
	}
	return result
}

func loadLayerMergeInputs(readers []io.ReaderAt) ([]*layerMergeInput, error, int) {
	var inputs []*layerMergeInput
	for idx, r := range readers {
		buf := make([]byte, HeaderV3LenBytes)
		if _, err := r.ReadAt(buf, 0); err != nil {
			return nil, err, idx
		}
		h, err := DeserializeHeader(buf)
		if err != nil {
			return nil, err, idx
		}
		if h.TileType != Mvt {
			return nil, fmt.Errorf("layer merging requires mvt tiles, got %s", tileTypeToString(h.TileType)), idx
		}
		if idx > 0 && h.TileCompression != inputs[0].header.TileCompression {
			c1, _ := compressionToString(h.TileCompression)
			c2, _ := compressionToString(inputs[0].header.TileCompression)
			return nil, fmt.Errorf("tile compression %s does not match %s", c1, c2), idx
		}
		if h.TileCompression != NoCompression && h.TileCompression != Gzip {
			c, _ := compressionToString(h.TileCompression)
			return nil, fmt.Errorf("tile compression %s not supported for layer merging", c), idx
		}

		metadata, err := DeserializeMetadata(io.NewSectionReader(r, int64(h.MetadataOffset), int64(h.MetadataLength)), h.InternalCompression)
		if err != nil {
			return nil, fmt.Errorf("reading metadata: %w", err), idx
		}

		var entries []EntryV3
		err = IterateEntries(h,
			func(offset uint64, length uint64) ([]byte, error) {
				b := make([]byte, length)
				_, err := r.ReadAt(b, int64(offset))
				return b, err
			},
			func(e EntryV3) {
				entries = append(entries, e)
			})
		if err != nil {
			return nil, err, idx
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].TileID < entries[j].TileID })
		if len(entries) == 0 {
			return nil, errors.New("archive has no tiles"), idx
		}

		inputs = append(inputs, &layerMergeInput{reader: r, header: h, metadata: metadata, entries: entries})
	}

	// fail fast if the declared layers overlap. tiles are checked individually during the merge.
	seen := make(map[string]int)
	for idx, in := range inputs {
		ids, _ := vectorLayerIDs(in.metadata)
		for _, id := range ids {
			if other, ok := seen[id]; ok && other != idx {
				return nil, fmt.Errorf("vector layer %q is also present in input %d; layers must be disjoint", id, other+1), idx
			}
			seen[id] = idx
		}
	}
	return inputs, nil, 0
}

// nextSegment returns the next segment at or after t, or ok=false when all inputs are exhausted.
func nextSegment(inputs []*layerMergeInput, t uint64) (seg layerSegment, ok bool) {
	for {
		// skip entries that end at or before t
		next := uint64(math.MaxUint64)
		for _, in := range inputs {
			for in.pos < len(in.entries) && in.entries[in.pos].TileID+uint64(in.entries[in.pos].RunLength) <= t {
				in.pos++
			}
			if in.pos < len(in.entries) {
				start := max(in.entries[in.pos].TileID, t)
				next = min(next, start)
			}
		}
		if next == math.MaxUint64 {
			return layerSegment{}, false
		}
		t = next

		end := uint64(math.MaxUint64)
		var sources []int
		for idx, in := range inputs {
			if in.pos >= len(in.entries) {
				continue
			}
			e := in.entries[in.pos]
			if e.TileID <= t {
				sources = append(sources, idx)
				end = min(end, e.TileID+uint64(e.RunLength))
			} else {
				end = min(end, e.TileID)
			}
		}
		if len(sources) > 0 {
			return layerSegment{start: t, end: end, sources: sources}, true
		}
	}
}

func readTile(in *layerMergeInput, e EntryV3) ([]byte, error) {
	b := make([]byte, e.Length)
	_, err := in.reader.ReadAt(b, int64(in.header.TileDataOffset+e.Offset))
	return b, err
}

// mergeLayerTiles writes the merged tile data section to tileData, returning the output
// header (without section offsets), merged metadata and directory entries.
func mergeLayerTiles(logger *log.Logger, readers []io.ReaderAt, names []string, tileData io.Writer) (HeaderV3, map[string]interface{}, []EntryV3, error) {
	var header HeaderV3
	inputs, err, errIdx := loadLayerMergeInputs(readers)
	if err != nil {
		return header, nil, nil, fmt.Errorf("%s: %w", names[errIdx], err)
	}

	compression := inputs[0].header.TileCompression
	maxZooms := make(map[uint8]bool)
	for _, in := range inputs {
		lastEntry := in.entries[len(in.entries)-1]
		z, _, _ := IDToZxy(lastEntry.TileID + uint64(lastEntry.RunLength) - 1)
		maxZooms[z] = true
	}
	if len(maxZooms) > 1 {
		var parts []string
		for idx, in := range inputs {
			lastEntry := in.entries[len(in.entries)-1]
			z, _, _ := IDToZxy(lastEntry.TileID + uint64(lastEntry.RunLength) - 1)
			parts = append(parts, fmt.Sprintf("%s=z%d", names[idx], z))
		}
		logger.Printf("WARNING: inputs have different max zooms (%s); clients will not overzoom the lower ones", strings.Join(parts, ", "))
	}

	// output dedup is keyed by the tuple of source offsets that produced a tile.
	written := make(map[string]offsetLen)
	var entries []EntryV3
	var offset uint64
	var addressed uint64
	var mergedTiles uint64
	var key []byte

	t := uint64(0)
	for {
		seg, ok := nextSegment(inputs, t)
		if !ok {
			break
		}
		t = seg.end

		key = key[:0]
		for _, idx := range seg.sources {
			key = binary.AppendUvarint(key, uint64(idx))
			key = binary.AppendUvarint(key, inputs[idx].entries[inputs[idx].pos].Offset)
		}

		loc, ok := written[string(key)]
		if !ok {
			var data []byte
			if len(seg.sources) == 1 {
				in := inputs[seg.sources[0]]
				data, err = readTile(in, in.entries[in.pos])
				if err != nil {
					return header, nil, nil, err
				}
			} else {
				var combined []byte
				layerOwner := make(map[string]int)
				for _, idx := range seg.sources {
					in := inputs[idx]
					raw, err := readTile(in, in.entries[in.pos])
					if err != nil {
						return header, nil, nil, err
					}
					part, err := decompressTile(raw, compression)
					if err != nil {
						return header, nil, nil, fmt.Errorf("%s: %w", names[idx], err)
					}
					layerNames, err := mvtLayerNames(part)
					if err != nil {
						z, x, y := IDToZxy(seg.start)
						return header, nil, nil, fmt.Errorf("%s: tile %d %d %d: %w", names[idx], z, x, y, err)
					}
					for _, name := range layerNames {
						if other, ok := layerOwner[name]; ok {
							z, x, y := IDToZxy(seg.start)
							return header, nil, nil, fmt.Errorf("layer %q appears in both %s and %s at tile %d %d %d; layers must be disjoint", name, names[other], names[idx], z, x, y)
						}
						layerOwner[name] = idx
					}
					combined = append(combined, part...)
				}
				data, err = compressTile(combined, compression)
				if err != nil {
					return header, nil, nil, err
				}
				mergedTiles += seg.end - seg.start
			}

			if _, err := tileData.Write(data); err != nil {
				return header, nil, nil, err
			}
			loc = offsetLen{offset, uint32(len(data))}
			written[string(key)] = loc
			offset += uint64(len(data))
		}

		runLength := seg.end - seg.start
		addressed += runLength
		if n := len(entries); n > 0 && entries[n-1].Offset == loc.Offset &&
			entries[n-1].TileID+uint64(entries[n-1].RunLength) == seg.start &&
			uint64(entries[n-1].RunLength)+runLength <= math.MaxUint32 {
			entries[n-1].RunLength += uint32(runLength)
		} else {
			if runLength > math.MaxUint32 {
				return header, nil, nil, errors.New("maximum 32-bit run length exceeded")
			}
			entries = append(entries, EntryV3{seg.start, loc.Offset, loc.Length, uint32(runLength)})
		}
	}

	logger.Printf("Combined layers in %d overlapping tiles", mergedTiles)

	var headers []HeaderV3
	var metadatas []map[string]interface{}
	for _, in := range inputs {
		headers = append(headers, in.header)
		metadatas = append(metadatas, in.metadata)
	}

	header.Clustered = true
	header.TileType = Mvt
	header.TileCompression = compression
	header.InternalCompression = Gzip
	header.TileDataLength = offset
	header.AddressedTilesCount = addressed
	header.TileEntriesCount = uint64(len(entries))
	header.TileContentsCount = uint64(len(written))
	firstZ, _, _ := IDToZxy(entries[0].TileID)
	last := entries[len(entries)-1]
	lastZ, _, _ := IDToZxy(last.TileID + uint64(last.RunLength) - 1)
	header.MinZoom = firstZ
	header.MaxZoom = lastZ
	header.MinLonE7, header.MinLatE7, header.MaxLonE7, header.MaxLatE7 = bounds(headers)
	header.CenterLonE7 = headers[0].CenterLonE7
	header.CenterLatE7 = headers[0].CenterLatE7
	header.CenterZoom = headers[0].CenterZoom

	return header, mergeLayerMetadata(metadatas), entries, nil
}

// mergeLayers merges MVT archives whose tiles may overlap but whose layers are disjoint.
func mergeLayers(logger *log.Logger, inputNames []string, output string) error {
	var readers []io.ReaderAt
	for _, name := range inputNames {
		f, err := os.Open(name)
		if err != nil {
			return err
		}
		defer f.Close()
		readers = append(readers, f)
	}

	tmpfile, err := os.CreateTemp(filepath.Dir(output), ".pmtiles-merge-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmpfile.Name())
	defer tmpfile.Close()

	header, metadata, entries, err := mergeLayerTiles(logger, readers, inputNames, tmpfile)
	if err != nil {
		return err
	}

	rootBytes, leavesBytes, _ := BuildDirectories(entries, 16384-HeaderV3LenBytes, Gzip)
	metadataBytes, err := SerializeMetadata(metadata, Gzip)
	if err != nil {
		return err
	}

	header.RootOffset = HeaderV3LenBytes
	header.RootLength = uint64(len(rootBytes))
	header.MetadataOffset = header.RootOffset + header.RootLength
	header.MetadataLength = uint64(len(metadataBytes))
	header.LeafDirectoryOffset = header.MetadataOffset + header.MetadataLength
	header.LeafDirectoryLength = uint64(len(leavesBytes))
	header.TileDataOffset = header.LeafDirectoryOffset + header.LeafDirectoryLength

	out, err := os.Create(output)
	if err != nil {
		return err
	}
	defer out.Close()

	for _, b := range [][]byte{SerializeHeader(header), rootBytes, metadataBytes, leavesBytes} {
		if _, err := out.Write(b); err != nil {
			return err
		}
	}
	if _, err := tmpfile.Seek(0, io.SeekStart); err != nil {
		return err
	}
	bar := defaultBytesProgressbar(logger, int64(header.TileDataLength), "writing tile data")
	if _, err := io.Copy(io.MultiWriter(out, bar), tmpfile); err != nil {
		return err
	}
	return out.Close()
}
