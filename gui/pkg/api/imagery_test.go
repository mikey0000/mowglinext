package api

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"maps"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newImageryTestServer(t *testing.T) (*gin.Engine, *imageryStore) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	s := newImageryStore(t.TempDir())
	r := gin.New()
	imageryRoutesWithStore(r.Group("/api"), s)
	return r, s
}

func uploadFile(t *testing.T, r *gin.Engine, filename string, data []byte, fields map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for k, v := range fields {
		require.NoError(t, mw.WriteField(k, v))
	}
	fw, err := mw.CreateFormFile("file", filename)
	require.NoError(t, err)
	_, err = fw.Write(data)
	require.NoError(t, err)
	require.NoError(t, mw.Close())
	req := httptest.NewRequest(http.MethodPost, "/api/mowglinext/imagery", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func waitReady(t *testing.T, s *imageryStore, name string) imageryOverlay {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		o, ok := s.get(name)
		require.True(t, ok)
		if o.Status != "processing" {
			return o
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("overlay %s still processing", name)
	return imageryOverlay{}
}

func imgReq(r *gin.Engine, method, path string, body any) *httptest.ResponseRecorder {
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func pngBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	img.SetNRGBA(1, 1, color.NRGBA{R: 200, A: 255})
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

// zipOf builds a zip of the given entries, in name order.
func zipOf(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range slices.Sorted(maps.Keys(entries)) {
		f, err := zw.Create(name)
		require.NoError(t, err)
		_, err = f.Write(entries[name])
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

func TestTileMath(t *testing.T) {
	lon, lat := tileFracToLonLat(0.5, 0.5, 0)
	assert.InDelta(t, 0, lon, 1e-12)
	assert.InDelta(t, 0, lat, 1e-12)
	// Known OSM tile at z=18 (Frankfurt): its north-west corner.
	lon, lat = tileFracToLonLat(137334, 88791, 18)
	assert.InDelta(t, 8.6, lon, 0.01)
	assert.InDelta(t, 50.1, lat, 0.01)
	assert.Equal(t, 0, tmsToXYZ(1, 1))
	assert.Equal(t, 7, tmsToXYZ(3, 0))
}

func TestParseTileCoords(t *testing.T) {
	z, x, y, ok := parseTileCoords("20", "549321", "378575.png")
	require.True(t, ok)
	assert.Equal(t, []int{20, 549321, 378575}, []int{z, x, y})
	for _, c := range [][3]string{
		{"-1", "0", "0"}, {"31", "0", "0"}, {"2", "4", "0"}, {"2", "0", "4"},
		{"+1", "0", "0"}, {"1", " 0", "0"}, {"", "0", "0"}, {"1", "0", ".png"},
		{"1", "0x1", "0"}, {"1", "0", "99999999999"},
	} {
		_, _, _, ok := parseTileCoords(c[0], c[1], c[2])
		assert.False(t, ok, "%v", c)
	}
}

func TestReorderOverlays(t *testing.T) {
	ov := []*imageryOverlay{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	got := reorderOverlays(ov, []string{"c", "x", "a", "c"})
	names := []string{}
	for _, o := range got {
		names = append(names, o.Name)
	}
	assert.Equal(t, []string{"c", "a", "b"}, names)
}

func TestImageryUniqueName(t *testing.T) {
	s := newImageryStore(t.TempDir())
	assert.Equal(t, "drone-june-2026", s.uniqueNameLocked("Drone June 2026!"))
	assert.Equal(t, "tiles", s.uniqueNameLocked("../../"))
	s.overlays = []*imageryOverlay{{Name: "garden"}}
	s.cancels["garden-2"] = func() {}
	assert.Equal(t, "garden-3", s.uniqueNameLocked("Garden"))
	assert.Regexp(t, imageryNameRe, s.uniqueNameLocked(string(bytes.Repeat([]byte("a-"), 60))))
}

func TestProcessXYZZipTMSFlip(t *testing.T) {
	dir := t.TempDir()
	zp := filepath.Join(dir, "t.zip")
	require.NoError(t, os.WriteFile(zp, zipOf(t, map[string][]byte{
		"site/tilemapresource.xml":    []byte("<TileMap/>"),
		"site/20/549321/670000.png":   pngBytes(t),
		"site/19/274660/335000.png":   pngBytes(t),
		"site/19/274660/335000.jpg":   pngBytes(t), // a second format: ignored
		"../../evil/1/0/0.png":        pngBytes(t), // only the z/x/y suffix is used
		"site/2/9/0.png":              pngBytes(t), // x outside the z=2 grid
		"site/readme.txt":             []byte("hi"),
		"site/19/274660/notanum.png":  pngBytes(t),
		"site/19/274660/335001.png/x": pngBytes(t),
	}), 0o644))

	tiles := filepath.Join(dir, "tiles")
	res, err := processXYZZip(context.Background(), zp, tiles, "auto", func(float64) {})
	require.NoError(t, err)
	assert.Equal(t, 1, res.minZoom)
	assert.Equal(t, 20, res.maxZoom)
	assert.Equal(t, ".png", res.tileExt)
	require.NotNil(t, res.bounds)
	assert.Less(t, res.bounds[0], res.bounds[2])
	assert.Less(t, res.bounds[1], res.bounds[3])
	// The z=1 tile (0, 1) is the whole south-west quarter: the bounds cover it.
	assert.InDelta(t, -180, res.bounds[0], 1e-9)
	assert.InDelta(t, -85.0511, res.bounds[1], 1e-3)
	assert.FileExists(t, tilePath(tiles, 20, 549321, tmsToXYZ(20, 670000), ".png"))
	assert.FileExists(t, tilePath(tiles, 1, 0, tmsToXYZ(1, 0), ".png"))
	assert.NoDirExists(t, filepath.Join(dir, "evil"))
	assert.NoDirExists(t, filepath.Join(tiles, "2"))

	// Explicit xyz: no flip.
	tiles2 := filepath.Join(dir, "tiles2")
	_, err = processXYZZip(context.Background(), zp, tiles2, "xyz", func(float64) {})
	require.NoError(t, err)
	assert.FileExists(t, tilePath(tiles2, 20, 549321, 670000, ".png"))
}

func TestProcessXYZZipRejectsEmpty(t *testing.T) {
	dir := t.TempDir()
	zp := filepath.Join(dir, "t.zip")
	require.NoError(t, os.WriteFile(zp, zipOf(t, map[string][]byte{"a.txt": []byte("x")}), 0o644))
	_, err := processXYZZip(context.Background(), zp, filepath.Join(dir, "tiles"), "", func(float64) {})
	assert.ErrorContains(t, err, "no z/x/y tiles")
}

func TestImageryUploadTileRouteAndDelete(t *testing.T) {
	r, s := newImageryTestServer(t)
	data := zipOf(t, map[string][]byte{"18/137334/88791.png": pngBytes(t)})
	w := uploadFile(t, r, "garden.zip", data, map[string]string{"label": "Drone June", "scheme": "xyz"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var o imageryOverlay
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &o))
	assert.Equal(t, "drone-june", o.Name)
	assert.Equal(t, "Drone June", o.Label)
	assert.Equal(t, "xyz", o.Kind)
	o = waitReady(t, s, o.Name)
	require.Equal(t, "ready", o.Status, o.Error)
	assert.Equal(t, 18, o.MaxZoom)
	assert.NoFileExists(t, filepath.Join(s.root, o.Name, "source.zip"), "upload is deleted once unpacked")

	// Persisted, and listed.
	assert.Len(t, newImageryStore(s.root).list(), 1)
	w = imgReq(r, http.MethodGet, "/api/mowglinext/imagery", nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"drone-june"`)

	w = imgReq(r, http.MethodGet, "/api/mowglinext/tiles/drone-june/18/137334/88791.png?v=1", nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "image/png", w.Header().Get("Content-Type"))
	assert.Equal(t, "public, max-age=604800", w.Header().Get("Cache-Control"))
	_, err := png.Decode(bytes.NewReader(w.Body.Bytes()))
	require.NoError(t, err)

	// Outside the imagery: 204, never a 404.
	w = imgReq(r, http.MethodGet, "/api/mowglinext/tiles/drone-june/18/1/1.png", nil)
	assert.Equal(t, http.StatusNoContent, w.Code)
	// Traversal attempts do not even match the route.
	for _, p := range []string{
		"/api/mowglinext/tiles/..%2F..%2Fetc/1/1/1.png",
		"/api/mowglinext/tiles/drone-june/1/..%2F..%2F/1.png",
	} {
		w = imgReq(r, http.MethodGet, p, nil)
		assert.Equal(t, http.StatusNotFound, w.Code, p)
	}
	// Bad names / coordinates are refused before touching the filesystem.
	for _, p := range []string{
		"/api/mowglinext/tiles/..etc/1/1/1.png",
		"/api/mowglinext/tiles/drone-june/1/0/..png",
		"/api/mowglinext/tiles/drone-june/-1/0/0.png",
		"/api/mowglinext/tiles/drone-june/2/0/9.png",
		"/api/mowglinext/tiles/Drone-June/18/137334/88791.png",
	} {
		w = imgReq(r, http.MethodGet, p, nil)
		assert.Equal(t, http.StatusBadRequest, w.Code, p)
	}
	w = imgReq(r, http.MethodGet, "/api/mowglinext/tiles/nope/1/1/1.png", nil)
	assert.Equal(t, http.StatusNotFound, w.Code)

	// Opacity is clamped, visibility and label update.
	w = imgReq(r, http.MethodPatch, "/api/mowglinext/imagery/drone-june", map[string]any{"opacity": 1.7, "visible": false, "label": "  Lawn  "})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	o, _ = s.get("drone-june")
	assert.Equal(t, 1.0, o.Opacity)
	assert.False(t, o.Visible)
	assert.Equal(t, "Lawn", o.Label)
	w = imgReq(r, http.MethodPatch, "/api/mowglinext/imagery/nope", map[string]any{"opacity": 0.5})
	assert.Equal(t, http.StatusNotFound, w.Code)

	// Delete removes the files.
	w = imgReq(r, http.MethodDelete, "/api/mowglinext/imagery/drone-june", nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.NoDirExists(t, filepath.Join(s.root, "drone-june"))
	assert.Empty(t, s.list())
	w = imgReq(r, http.MethodDelete, "/api/mowglinext/imagery/drone-june", nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestImageryOrder(t *testing.T) {
	r, s := newImageryTestServer(t)
	data := zipOf(t, map[string][]byte{"1/0/0.png": pngBytes(t)})
	for range 2 {
		require.Equal(t, http.StatusOK, uploadFile(t, r, "garden.zip", data, nil).Code)
	}
	waitReady(t, s, "garden")
	waitReady(t, s, "garden-2")
	w := imgReq(r, http.MethodPost, "/api/mowglinext/imagery-order", map[string]any{"names": []string{"garden-2", "garden"}})
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "garden-2", s.list()[0].Name)
	assert.Equal(t, "garden-2", newImageryStore(s.root).list()[0].Name)
}

func TestImageryUploadErrors(t *testing.T) {
	r, s := newImageryTestServer(t)
	w := uploadFile(t, r, "virus.exe", []byte("MZ"), nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	// A broken zip is recorded as an error, and leaves no tiles behind.
	w = uploadFile(t, r, "broken.zip", []byte("not a zip"), nil)
	require.Equal(t, http.StatusOK, w.Code)
	o := waitReady(t, s, "broken")
	assert.Equal(t, "error", o.Status)
	assert.NotEmpty(t, o.Error)
	assert.NoDirExists(t, filepath.Join(s.root, "broken", "tiles"))

	req := httptest.NewRequest(http.MethodPost, "/api/mowglinext/imagery", bytes.NewReader([]byte("{}")))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestImageryInterruptedProcessingIsAnErrorAfterRestart(t *testing.T) {
	root := t.TempDir()
	list := []imageryOverlay{
		{Name: "a", Status: "processing"},
		{Name: "../evil", Status: "ready"},
	}
	b, err := json.Marshal(list)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "overlays.json"), b, 0o644))
	got := newImageryStore(root).list()
	require.Len(t, got, 1, "names that fail validation are dropped")
	assert.Equal(t, "error", got[0].Status)
}

func TestImageryDeleteWhileProcessing(t *testing.T) {
	r, s := newImageryTestServer(t)
	dir := filepath.Join(s.root, "x")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	src := filepath.Join(dir, "source.zip")
	require.NoError(t, os.WriteFile(src, zipOf(t, map[string][]byte{"1/0/0.png": pngBytes(t)}), 0o644))
	ctx, cancel := context.WithCancel(context.Background())
	s.overlays = []*imageryOverlay{{Name: "x", Status: "processing"}}
	s.cancels["x"] = cancel

	w := imgReq(r, http.MethodDelete, "/api/mowglinext/imagery/x", nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, s.list())
	assert.DirExists(t, dir, "the running conversion owns the folder until it stops")
	assert.Equal(t, "x-2", s.uniqueNameLocked("x"), "the name stays reserved meanwhile")

	s.process(ctx, "x", "xyz", src, "")
	assert.NoDirExists(t, dir)
	assert.Empty(t, s.cancels)
	assert.Empty(t, s.list())
}

func writeMBTiles(t *testing.T, path, format string, tiles [][3]int, data []byte) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE metadata (name text, value text);
		CREATE TABLE tiles (zoom_level integer, tile_column integer, tile_row integer, tile_data blob);`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO metadata VALUES ('format', ?)`, format)
	require.NoError(t, err)
	for _, tl := range tiles {
		_, err = db.Exec(`INSERT INTO tiles VALUES (?, ?, ?, ?)`, tl[0], tl[1], tl[2], data)
		require.NoError(t, err)
	}
}

func TestProcessMBTiles(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.mbtiles")
	writeMBTiles(t, p, "png", [][3]int{{21, 1098642, 1340000}, {20, 549321, 670000}, {2, 9, 0}}, pngBytes(t))
	before, err := os.ReadFile(p)
	require.NoError(t, err)

	tiles := filepath.Join(dir, "tiles")
	res, err := processMBTiles(context.Background(), p, tiles, func(float64) {})
	require.NoError(t, err)
	assert.Equal(t, 20, res.minZoom)
	assert.Equal(t, 21, res.maxZoom)
	assert.Equal(t, ".png", res.tileExt)
	assert.FileExists(t, tilePath(tiles, 21, 1098642, tmsToXYZ(21, 1340000), ".png"))
	assert.NoDirExists(t, filepath.Join(tiles, "2"), "tiles outside the grid are skipped")

	after, err := os.ReadFile(p)
	require.NoError(t, err)
	assert.Equal(t, before, after, "the MBTiles file is opened read-only")
}

func TestProcessMBTilesRejectsVectorTiles(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "v.mbtiles")
	writeMBTiles(t, p, "pbf", [][3]int{{14, 1, 1}}, []byte{1, 2, 3})
	_, err := processMBTiles(context.Background(), p, filepath.Join(dir, "tiles"), func(float64) {})
	assert.ErrorContains(t, err, "not a raster image format")
}

func TestImageryMBTilesUpload(t *testing.T) {
	r, s := newImageryTestServer(t)
	p := filepath.Join(t.TempDir(), "garden.mbtiles")
	writeMBTiles(t, p, "jpg", [][3]int{{18, 137334, tmsToXYZ(18, 88791)}}, pngBytes(t))
	data, err := os.ReadFile(p)
	require.NoError(t, err)
	w := uploadFile(t, r, "garden.mbtiles", data, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	o := waitReady(t, s, "garden")
	require.Equal(t, "ready", o.Status, o.Error)
	assert.Equal(t, "mbtiles", o.Kind)
	assert.Equal(t, ".jpg", o.TileExt)
	w = imgReq(r, http.MethodGet, "/api/mowglinext/tiles/garden/18/137334/88791.jpg", nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "image/jpeg", w.Header().Get("Content-Type"))
}
