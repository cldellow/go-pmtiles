package pmtiles

import (
	"github.com/stretchr/testify/assert"
	"os"
	"testing"
)

func TestCluster(t *testing.T) {
	fileToCluster := makeFixtureCopy(t, "unclustered", "cluster")

	err := Cluster(logger, fileToCluster, true)
	assert.Nil(t, err)

	file, err := os.OpenFile(fileToCluster, os.O_RDONLY, 0666)
	defer file.Close()
	assert.Nil(t, err)

	buf := make([]byte, HeaderV3LenBytes)
	_, _ = file.Read(buf)
	header, _ := DeserializeHeader(buf)
	assert.True(t, header.Clustered)
}

// Clustering rewrites the header; it must keep the tiles' compression (it
// doesn't recompress them). It used to set an MVT archive's to gzip, which
// left a zstd archive unreadable.
func TestClusterKeepsTileCompression(t *testing.T) {
	fileToCluster := makeFixtureCopy(t, "unclustered", "cluster-zstd")

	file, err := os.OpenFile(fileToCluster, os.O_RDWR, 0666)
	assert.Nil(t, err)
	buf := make([]byte, HeaderV3LenBytes)
	_, _ = file.Read(buf)
	header, err := DeserializeHeader(buf)
	assert.Nil(t, err)
	// The fixture's tiles are PNGs; cluster doesn't decode tiles, so call
	// them zstd vector tiles, as tilemaker writes them.
	header.TileType = Mvt
	header.TileCompression = Zstd
	_, err = file.WriteAt(SerializeHeader(header), 0)
	assert.Nil(t, err)
	file.Close()

	err = Cluster(logger, fileToCluster, true)
	assert.Nil(t, err)

	file, err = os.OpenFile(fileToCluster, os.O_RDONLY, 0666)
	defer file.Close()
	assert.Nil(t, err)
	_, _ = file.Read(buf)
	header, _ = DeserializeHeader(buf)
	assert.True(t, header.Clustered)
	assert.Equal(t, Compression(Zstd), header.TileCompression)
}
