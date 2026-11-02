package pmtiles

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
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

// Merging recompresses most tiles, so reuse gzip state: allocating a fresh
// writer per tile makes the merge spend most of its time in the allocator and GC.
var gzipReaders sync.Pool
var gzipWriters = sync.Pool{New: func() any {
	w, _ := gzip.NewWriterLevel(io.Discard, mergeGzipLevel)
	return w
}}

// klauspost's levels 1-6 (and DefaultCompression, which is 5) use fast
// encoders that made a merged basemap ~7% larger than zlib/libdeflate at
// level 6. Level 7 matches their size, at ~35% more compression time.
const mergeGzipLevel = 7

// zstd's encoder and decoder are safe for concurrent EncodeAll/DecodeAll
// calls, and keep their own pools of state.
var zstdEncoder, _ = zstd.NewWriter(nil,
	zstd.WithEncoderLevel(mergeZstdLevel),
	zstd.WithEncoderConcurrency(runtime.GOMAXPROCS(0)),
	zstd.WithZeroFrames(true))
var zstdDecoder, _ = zstd.NewReader(nil,
	zstd.WithDecoderConcurrency(runtime.GOMAXPROCS(0)),
	zstd.WithDecoderMaxMemory(1<<30))

// On a Nova Scotia basemap, klauspost's SpeedDefault (~zstd level 3, what
// tilemaker writes) was 4% bigger than gzip level 7; SpeedBetterCompression
// matches gzip's size, at about the same speed as SpeedDefault and ~30% faster
// than gzip. SpeedBestCompression is 1% smaller but 3x slower.
const mergeZstdLevel = zstd.SpeedBetterCompression

func decompressTile(data []byte, compression Compression) ([]byte, error) {
	switch compression {
	case NoCompression:
		return data, nil
	case Zstd:
		return zstdDecoder.DecodeAll(data, make([]byte, 0, 4*len(data)))
	case Gzip:
		var r *gzip.Reader
		if v := gzipReaders.Get(); v != nil {
			r = v.(*gzip.Reader)
			if err := r.Reset(bytes.NewReader(data)); err != nil {
				return nil, err
			}
		} else {
			var err error
			if r, err = gzip.NewReader(bytes.NewReader(data)); err != nil {
				return nil, err
			}
		}
		defer gzipReaders.Put(r)
		out := bytes.NewBuffer(make([]byte, 0, 4*len(data)))
		if _, err := out.ReadFrom(r); err != nil {
			return nil, err
		}
		return out.Bytes(), nil
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
		w := gzipWriters.Get().(*gzip.Writer)
		defer gzipWriters.Put(w)
		w.Reset(&b)
		if _, err := w.Write(data); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		return b.Bytes(), nil
	case Zstd:
		return zstdEncoder.EncodeAll(data, make([]byte, 0, len(data)/2+64)), nil
	default:
		c, _ := compressionToString(compression)
		return nil, fmt.Errorf("tile compression %s not supported for layer merging", c)
	}
}

// layerMergeCompressions are the tile compressions merging can read and write.
func layerMergeCompression(c Compression) bool {
	return c == NoCompression || c == Gzip || c == Zstd
}

// dedupeLayers keeps the first of several layer objects with the same key
// (e.g. "id" in vector_layers), widening its zoom range and adding any fields
// that only later declarations have. Several inputs may declare the same layer
// as long as they never put it in the same tile.
func dedupeLayers(layers []interface{}, key string) []interface{} {
	var result []interface{}
	byID := make(map[string]map[string]interface{})
	for _, l := range layers {
		m, ok := l.(map[string]interface{})
		if !ok {
			result = append(result, l)
			continue
		}
		id, ok := m[key].(string)
		if !ok {
			result = append(result, l)
			continue
		}
		first, seen := byID[id]
		if !seen {
			copied := make(map[string]interface{}, len(m))
			for k, v := range m {
				copied[k] = v
			}
			byID[id] = copied
			result = append(result, copied)
			continue
		}
		if z, ok := m["minzoom"].(float64); ok {
			if fz, ok := first["minzoom"].(float64); !ok || z < fz {
				first["minzoom"] = z
			}
		}
		if z, ok := m["maxzoom"].(float64); ok {
			if fz, ok := first["maxzoom"].(float64); !ok || z > fz {
				first["maxzoom"] = z
			}
		}
		if fields, ok := m["fields"].(map[string]interface{}); ok {
			firstFields, ok := first["fields"].(map[string]interface{})
			if !ok {
				firstFields = make(map[string]interface{})
			} else {
				copied := make(map[string]interface{}, len(firstFields))
				for k, v := range firstFields {
					copied[k] = v
				}
				firstFields = copied
			}
			for k, v := range fields {
				if _, ok := firstFields[k]; !ok {
					firstFields[k] = v
				}
			}
			first["fields"] = firstFields
		}
	}
	return result
}

// mergeLayerMetadata copies the first input's metadata, combining vector_layers
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
		result["vector_layers"] = dedupeLayers(vectorLayers, "id")
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
		statLayers = dedupeLayers(statLayers, "layer")
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
		// Inputs may differ in tile compression: each is decompressed with its own.
		if !layerMergeCompression(h.TileCompression) {
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

	// Several inputs may declare the same layer (e.g. a layer split across two
	// tilemaker configs by area), so the metadata isn't checked here. Each
	// combined tile is checked for a layer coming from two inputs instead.
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

// A run of tile IDs to emit, and the source tiles that make it up.
type mergeJob struct {
	seg   layerSegment
	ents  []EntryV3 // the covering entry of each source, in seg.sources order
	id    uint32    // index of this output tile's contents, in production order
	isNew bool      // whether this job produces id (else it reuses an earlier job's)
	data  []byte    // the output tile; nil if it's empty
	err   error
	done  chan struct{}
}

type mergeStats struct {
	bytes, addressed, combined, empty uint64
}

// noTile marks contents that turned out to be empty, and so aren't written.
var noTile = offsetLen{Offset: math.MaxUint64}

// buildTile reads a job's source tiles and produces its output tile.
func buildTile(inputs []*layerMergeInput, names []string, compression Compression, j *mergeJob) ([]byte, error) {
	if len(j.seg.sources) == 1 {
		in := inputs[j.seg.sources[0]]
		raw, err := readTile(in, j.ents[0])
		if err != nil {
			return nil, err
		}
		if in.header.TileCompression != compression {
			part, err := decompressTile(raw, in.header.TileCompression)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", names[j.seg.sources[0]], err)
			}
			if len(part) == 0 {
				return nil, nil
			}
			return compressTile(part, compression)
		}
		// tilemaker writes empty tiles, which gzip to ~20 bytes (zstd to ~9).
		// Drop them, as tile-join and tile-smush do.
		if len(raw) < 64 {
			part, err := decompressTile(raw, compression)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", names[j.seg.sources[0]], err)
			}
			if len(part) == 0 {
				return nil, nil
			}
		}
		return raw, nil
	}

	// An MVT Tile is a list of layers, so concatenating tiles merges them.
	var combined []byte
	var layerOwner map[string]int
	for i, idx := range j.seg.sources {
		raw, err := readTile(inputs[idx], j.ents[i])
		if err != nil {
			return nil, err
		}
		part, err := decompressTile(raw, inputs[idx].header.TileCompression)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", names[idx], err)
		}
		layerNames, err := mvtLayerNames(part)
		if err != nil {
			z, x, y := IDToZxy(j.seg.start)
			return nil, fmt.Errorf("%s: tile %d %d %d: %w", names[idx], z, x, y, err)
		}
		if layerOwner == nil {
			layerOwner = make(map[string]int, 8)
		}
		for _, name := range layerNames {
			if other, ok := layerOwner[name]; ok {
				z, x, y := IDToZxy(j.seg.start)
				return nil, fmt.Errorf("layer %q appears in both %s and %s at tile %d %d %d; layers must be disjoint", name, names[other], names[idx], z, x, y)
			}
			layerOwner[name] = idx
		}
		combined = append(combined, part...)
	}
	if len(combined) == 0 {
		return nil, nil
	}
	return compressTile(combined, compression)
}

// mergeSegments writes the merged tile data to tileData in tile ID order, and
// returns its directory entries and the number of unique tiles written.
//
// One goroutine walks the inputs in tile ID order and hands out jobs; workers
// read, combine and compress tiles in parallel; this goroutine writes their
// results in the order the jobs were made.
//
// Output tiles are deduplicated by the source tiles they're built from (not by
// content), so no tile needs hashing: tilemaker already dedupes identical tiles
// within each input, e.g. the ocean.
func mergeSegments(inputs []*layerMergeInput, names []string, compression Compression, workers int, tileData io.Writer) ([]EntryV3, uint64, mergeStats, error) {
	var stats mergeStats
	queue := make(chan *mergeJob, 64*workers) // bounds memory held by jobs in flight
	work := make(chan *mergeJob, 64*workers)
	abort := make(chan struct{})

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range work {
				j.data, j.err = buildTile(inputs, names, compression, j)
				close(j.done)
			}
		}()
	}

	go func() {
		defer close(queue)
		defer close(work)
		// Keyed by (input, offset) for the common single-source case, which
		// keeps this small for archives with tens of millions of tiles.
		single := make(map[uint64]uint32)
		multi := make(map[string]uint32)
		var key []byte
		var next uint32
		t := uint64(0)
		for {
			seg, ok := nextSegment(inputs, t)
			if !ok {
				return
			}
			t = seg.end
			j := &mergeJob{seg: seg, ents: make([]EntryV3, len(seg.sources)), done: make(chan struct{})}
			for i, idx := range seg.sources {
				j.ents[i] = inputs[idx].entries[inputs[idx].pos]
			}

			var id uint32
			var seen bool
			if len(seg.sources) == 1 && j.ents[0].Offset < 1<<56 {
				k := uint64(seg.sources[0])<<56 | j.ents[0].Offset
				if id, seen = single[k]; !seen {
					id = next
					single[k] = id
				}
			} else {
				key = key[:0]
				for i, idx := range seg.sources {
					key = binary.AppendUvarint(key, uint64(idx))
					key = binary.AppendUvarint(key, j.ents[i].Offset)
				}
				if id, seen = multi[string(key)]; !seen {
					id = next
					multi[string(key)] = id
				}
			}
			j.id = id
			if seen {
				close(j.done)
			} else {
				j.isNew = true
				next++
				select {
				case work <- j:
				case <-abort:
					return
				}
			}
			select {
			case queue <- j:
			case <-abort:
				return
			}
		}
	}()

	// locs[id] is where the contents with that id were written.
	var locs []offsetLen
	var entries []EntryV3
	var contents uint64
	var err error
	for j := range queue {
		<-j.done
		if err == nil && j.err != nil {
			err = j.err
			close(abort)
		}
		if err != nil {
			continue // drain, so the producer and workers can exit
		}

		runLength := j.seg.end - j.seg.start
		if j.isNew {
			loc := noTile
			if j.data != nil {
				if _, werr := tileData.Write(j.data); werr != nil {
					err = werr
					close(abort)
					continue
				}
				loc = offsetLen{stats.bytes, uint32(len(j.data))}
				stats.bytes += uint64(len(j.data))
				contents++
				if len(j.seg.sources) > 1 {
					stats.combined += runLength
				}
			}
			locs = append(locs, loc)
			j.data = nil
		}
		loc := locs[j.id]
		if loc == noTile {
			stats.empty += runLength
			continue
		}

		stats.addressed += runLength
		if n := len(entries); n > 0 && entries[n-1].Offset == loc.Offset &&
			entries[n-1].TileID+uint64(entries[n-1].RunLength) == j.seg.start &&
			uint64(entries[n-1].RunLength)+runLength <= math.MaxUint32 {
			entries[n-1].RunLength += uint32(runLength)
		} else {
			if runLength > math.MaxUint32 {
				err = errors.New("maximum 32-bit run length exceeded")
				close(abort)
				continue
			}
			entries = append(entries, EntryV3{j.seg.start, loc.Offset, loc.Length, uint32(runLength)})
		}
	}
	wg.Wait()
	return entries, contents, stats, err
}

// mergeLayerTiles writes the merged tile data section to tileData, returning the output
// header (without section offsets), merged metadata and directory entries.
//
// Tiles are written with the given compression; UnknownCompression means the
// inputs' compression, if they all agree, and gzip otherwise.
func mergeLayerTiles(logger *log.Logger, readers []io.ReaderAt, names []string, compression Compression, tileData io.Writer) (HeaderV3, map[string]interface{}, []EntryV3, error) {
	var header HeaderV3
	inputs, err, errIdx := loadLayerMergeInputs(readers)
	if err != nil {
		return header, nil, nil, fmt.Errorf("%s: %w", names[errIdx], err)
	}

	if compression == UnknownCompression {
		compression = inputs[0].header.TileCompression
		for _, in := range inputs {
			if in.header.TileCompression != compression {
				compression = Gzip
				break
			}
		}
	}
	if !layerMergeCompression(compression) {
		c, _ := compressionToString(compression)
		return header, nil, nil, fmt.Errorf("tile compression %s not supported for layer merging", c)
	}
	var transcoded []string
	for idx, in := range inputs {
		if in.header.TileCompression != compression {
			c, _ := compressionToString(in.header.TileCompression)
			transcoded = append(transcoded, fmt.Sprintf("%s (%s)", names[idx], c))
		}
	}
	if len(transcoded) > 0 {
		c, _ := compressionToString(compression)
		logger.Printf("Recompressing tiles to %s from %s", c, strings.Join(transcoded, ", "))
	}
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

	start := time.Now()
	entries, contents, stats, err := mergeSegments(inputs, names, compression, mergeWorkers(), tileData)
	if err != nil {
		return header, nil, nil, err
	}
	if len(entries) == 0 {
		return header, nil, nil, errors.New("merged archive has no tiles")
	}
	offset, addressed, mergedTiles := stats.bytes, stats.addressed, stats.combined
	logger.Printf("Merged %d tiles (%d unique) in %v; dropped %d empty tiles", addressed, contents, time.Since(start).Round(time.Millisecond), stats.empty)

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
	header.TileContentsCount = contents
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
//
// The tile data is written straight into the output, after a reserved first
// 16 KiB; the metadata and leaf directories follow it, and the header and root
// directory are written into the reserved space last. The spec only requires
// the header to come first and the root directory to end within 16 KiB, and
// this avoids writing all the tile data to a temporary file and copying it.
func mergeLayers(logger *log.Logger, inputNames []string, output string, compression Compression) error {
	var readers []io.ReaderAt
	for _, name := range inputNames {
		f, err := os.Open(name)
		if err != nil {
			return err
		}
		defer f.Close()
		if mergeReadsRandom() {
			adviseRandom(f)
		}
		readers = append(readers, f)
	}

	out, err := os.Create(output)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		out.Close()
		if !ok {
			os.Remove(output)
		}
	}()

	const reserved = 16384
	if _, err := out.Seek(reserved, io.SeekStart); err != nil {
		return err
	}
	tileData := bufio.NewWriterSize(out, 8<<20)
	header, metadata, entries, err := mergeLayerTiles(logger, readers, inputNames, compression, tileData)
	if err != nil {
		return err
	}
	if err := tileData.Flush(); err != nil {
		return err
	}

	rootBytes, leavesBytes, _ := BuildDirectories(entries, reserved-HeaderV3LenBytes, Gzip)
	metadataBytes, err := SerializeMetadata(metadata, Gzip)
	if err != nil {
		return err
	}

	header.RootOffset = HeaderV3LenBytes
	header.RootLength = uint64(len(rootBytes))
	header.TileDataOffset = reserved
	header.MetadataOffset = header.TileDataOffset + header.TileDataLength
	header.MetadataLength = uint64(len(metadataBytes))
	header.LeafDirectoryOffset = header.MetadataOffset + header.MetadataLength
	header.LeafDirectoryLength = uint64(len(leavesBytes))

	// The file position is now the end of the tile data.
	for _, b := range [][]byte{metadataBytes, leavesBytes} {
		if _, err := out.Write(b); err != nil {
			return err
		}
	}
	if _, err := out.WriteAt(append(SerializeHeader(header), rootBytes...), 0); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}

// mergeWorkers is how many tiles are read, combined and compressed at once.
// PMTILES_MERGE_WORKERS overrides it (for benchmarking).
func mergeWorkers() int {
	if n, err := strconv.Atoi(os.Getenv("PMTILES_MERGE_WORKERS")); err == nil && n > 0 {
		return n
	}
	return runtime.GOMAXPROCS(0)
}

// mergeReadsRandom says whether to tell the kernel the inputs are read at
// random, which turns off readahead. PMTILES_MERGE_FADVISE=random enables it
// (for benchmarking).
func mergeReadsRandom() bool {
	return os.Getenv("PMTILES_MERGE_FADVISE") == "random"
}
