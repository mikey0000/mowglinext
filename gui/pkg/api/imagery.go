package api

// Custom map tiles (drone orthomosaics, pre-tiled XYZ sets) drawn as extra
// raster layers above the Mapbox base on the Map page.
//
// Storage: $MAP_TILES_DIR (default /ros2_ws/maps/map_tiles, on the
// mowgli_maps volume the GUI already mounts and the updater already backs up)
// holds overlays.json (the layer list, bottom-most first) plus one folder per
// overlay:
//
//	<name>/tiles/<z>/<x>/<y>.<png|jpg|webp>   XYZ tiles (Google/OSM row order)
//
// An overlay is created by uploading a .zip of a z/x/y tile folder (QGIS
// "Generate XYZ tiles (Directory)", gdal2tiles). The zip is unpacked in the
// background and deleted; only the tiles are kept.

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	imageryMaxUploadBytes = 300 << 20
	// imageryMaxTileBytes caps one unpacked tile; real tiles are a few hundred kB.
	imageryMaxTileBytes = 16 << 20
	// imageryMaxUnpackedBytes caps the total unpacked size of one overlay, so a
	// zip bomb cannot fill the robot's storage.
	imageryMaxUnpackedBytes = 1 << 30
	imageryMaxZoom          = 30
	imageryMaxLabelLen      = 128
)

type imageryOverlay struct {
	Name      string      `json:"name"`
	Label     string      `json:"label"`
	Kind      string      `json:"kind"`
	Status    string      `json:"status"` // processing | ready | error
	Error     string      `json:"error,omitempty"`
	Progress  float64     `json:"progress"`
	Opacity   float64     `json:"opacity"`
	Visible   bool        `json:"visible"`
	MinZoom   int         `json:"minZoom,omitempty"`
	MaxZoom   int         `json:"maxZoom,omitempty"`
	Bounds    *[4]float64 `json:"bounds,omitempty"` // west,south,east,north
	TileExt   string      `json:"tileExt,omitempty"`
	SizeBytes int64       `json:"sizeBytes"`
	CreatedAt int64       `json:"createdAt"`
}

type imageryStore struct {
	root     string
	mu       sync.Mutex
	overlays []*imageryOverlay
	// cancels holds the running conversions. An entry stays until its
	// goroutine has finished (even after a delete) so the name is not reused
	// while that goroutine may still write into the folder.
	cancels map[string]context.CancelFunc
}

var (
	// imageryNameRe is the only shape of overlay name the API accepts, so a
	// name can never point outside the tiles directory.
	imageryNameRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	imagerySlugRe   = regexp.MustCompile(`[^a-z0-9_-]+`)
	imageryTileExts = map[string]bool{".png": true, ".jpg": true, ".webp": true}
)

func newImageryStore(root string) *imageryStore {
	s := &imageryStore{root: root, cancels: map[string]context.CancelFunc{}}
	b, err := os.ReadFile(filepath.Join(root, "overlays.json"))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("imagery: read overlays.json: %v", err)
		}
		return s
	}
	var list []*imageryOverlay
	if err := json.Unmarshal(b, &list); err != nil {
		log.Printf("imagery: bad overlays.json: %v", err)
		return s
	}
	for _, o := range list {
		if !imageryNameRe.MatchString(o.Name) {
			continue
		}
		if o.Status == "processing" {
			o.Status = "error"
			o.Error = "processing was interrupted (GUI restarted); delete and upload again"
		}
		s.overlays = append(s.overlays, o)
	}
	return s
}

// saveLocked persists the overlay list atomically. Caller holds s.mu.
func (s *imageryStore) saveLocked() error {
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		return err
	}
	if s.overlays == nil {
		s.overlays = []*imageryOverlay{}
	}
	b, err := json.MarshalIndent(s.overlays, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.root, ".overlays.json.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.root, "overlays.json"))
}

func (s *imageryStore) list() []imageryOverlay {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]imageryOverlay, 0, len(s.overlays))
	for _, o := range s.overlays {
		out = append(out, *o)
	}
	return out
}

func (s *imageryStore) findLocked(name string) (int, *imageryOverlay) {
	for i, o := range s.overlays {
		if o.Name == name {
			return i, o
		}
	}
	return -1, nil
}

func (s *imageryStore) get(name string) (imageryOverlay, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, o := s.findLocked(name)
	if o == nil {
		return imageryOverlay{}, false
	}
	return *o, true
}

func (s *imageryStore) update(name string, fn func(o *imageryOverlay)) (imageryOverlay, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, o := s.findLocked(name)
	if o == nil {
		return imageryOverlay{}, os.ErrNotExist
	}
	fn(o)
	return *o, s.saveLocked()
}

// setProgress updates progress in memory only (persisted on completion).
func (s *imageryStore) setProgress(name string, p float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, o := s.findLocked(name); o != nil {
		o.Progress = p
	}
}

// uniqueNameLocked turns a label into an overlay name that is not in use.
func (s *imageryStore) uniqueNameLocked(label string) string {
	base := imagerySlugRe.ReplaceAllString(strings.ToLower(label), "-")
	base = strings.Trim(base, "-_")
	if len(base) > 48 {
		base = strings.Trim(base[:48], "-_")
	}
	if base == "" {
		base = "tiles"
	}
	name := base
	for i := 2; ; i++ {
		_, o := s.findLocked(name)
		_, busy := s.cancels[name]
		if _, err := os.Stat(filepath.Join(s.root, name)); o == nil && !busy && errors.Is(err, os.ErrNotExist) {
			return name
		}
		name = fmt.Sprintf("%s-%d", base, i)
	}
}

func imageryKindForFile(filename string) (kind, ext string, err error) {
	ext = strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".zip":
		return "xyz", ext, nil
	}
	return "", ext, fmt.Errorf("unsupported file type %q (upload a .zip of an XYZ tile folder)", ext)
}

// ---------------------------------------------------------------------------
// Routes
// ---------------------------------------------------------------------------

func imageryRoot() string {
	if d := os.Getenv("MAP_TILES_DIR"); d != "" {
		return d
	}
	return "/ros2_ws/maps/map_tiles"
}

// ImageryRoutes registers the custom map tiles API under /mowglinext.
func ImageryRoutes(r *gin.RouterGroup) {
	imageryRoutesWithStore(r, newImageryStore(imageryRoot()))
}

func imageryRoutesWithStore(r *gin.RouterGroup, s *imageryStore) {
	g := r.Group("/mowglinext")
	g.GET("/imagery", s.handleList)
	g.POST("/imagery", s.handleUpload)
	g.PATCH("/imagery/:name", s.handlePatch)
	g.DELETE("/imagery/:name", s.handleDelete)
	g.POST("/imagery-order", s.handleOrder)
	g.GET("/tiles/:name/:z/:x/:y", s.handleTile)
}

// handleList lists the custom map tile overlays.
//
// @Summary list custom map tile overlays (bottom-most first)
// @Tags imagery
// @Produce json
// @Router /mowglinext/imagery [get]
func (s *imageryStore) handleList(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"overlays": s.list()})
}

// handlePatch updates the label, opacity or visibility of an overlay.
//
// @Summary update an overlay's label, opacity or visibility
// @Tags imagery
// @Accept json
// @Produce json
// @Param name path string true "overlay name"
// @Success 200 {object} imageryOverlay
// @Failure 400 {object} ErrorResponse
// @Failure 404 {object} ErrorResponse
// @Router /mowglinext/imagery/{name} [patch]
func (s *imageryStore) handlePatch(c *gin.Context) {
	var req struct {
		Label   *string  `json:"label"`
		Opacity *float64 `json:"opacity"`
		Visible *bool    `json:"visible"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		return
	}
	if req.Opacity != nil && (math.IsNaN(*req.Opacity) || math.IsInf(*req.Opacity, 0)) {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid opacity"})
		return
	}
	o, err := s.update(c.Param("name"), func(o *imageryOverlay) {
		if req.Label != nil {
			if l := cleanImageryLabel(*req.Label); l != "" {
				o.Label = l
			}
		}
		if req.Opacity != nil {
			o.Opacity = math.Max(0, math.Min(1, *req.Opacity))
		}
		if req.Visible != nil {
			o.Visible = *req.Visible
		}
	})
	if errors.Is(err, os.ErrNotExist) {
		c.JSON(http.StatusNotFound, ErrorResponse{Error: "no such overlay"})
		return
	} else if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	c.JSON(http.StatusOK, o)
}

// handleOrder reorders the overlays.
//
// @Summary reorder overlays: body {"names": [...]}, first = bottom-most
// @Tags imagery
// @Accept json
// @Produce json
// @Router /mowglinext/imagery-order [post]
func (s *imageryStore) handleOrder(c *gin.Context) {
	var req struct {
		Names []string `json:"names"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		return
	}
	s.mu.Lock()
	s.overlays = reorderOverlays(s.overlays, req.Names)
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"overlays": s.list()})
}

// handleDelete removes an overlay and its tiles.
//
// @Summary delete an overlay and its tiles
// @Tags imagery
// @Produce json
// @Param name path string true "overlay name"
// @Success 200 {object} OkResponse
// @Failure 404 {object} ErrorResponse
// @Router /mowglinext/imagery/{name} [delete]
func (s *imageryStore) handleDelete(c *gin.Context) {
	name := c.Param("name")
	if !imageryNameRe.MatchString(name) {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid name"})
		return
	}
	s.mu.Lock()
	i, o := s.findLocked(name)
	if o == nil {
		s.mu.Unlock()
		c.JSON(http.StatusNotFound, ErrorResponse{Error: "no such overlay"})
		return
	}
	s.overlays = append(s.overlays[:i], s.overlays[i+1:]...)
	err := s.saveLocked()
	cancel, processing := s.cancels[name]
	if processing {
		cancel() // the conversion goroutine removes the folder when it stops
	} else if rmErr := os.RemoveAll(filepath.Join(s.root, name)); rmErr != nil && err == nil {
		err = rmErr
	}
	s.mu.Unlock()
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	c.JSON(http.StatusOK, OkResponse{})
}

// reorderOverlays returns overlays ordered by names; unknown names are
// ignored and overlays missing from names keep their relative order at the end.
func reorderOverlays(overlays []*imageryOverlay, names []string) []*imageryOverlay {
	out := make([]*imageryOverlay, 0, len(overlays))
	used := map[string]bool{}
	for _, n := range names {
		for _, o := range overlays {
			if o.Name == n && !used[n] {
				out = append(out, o)
				used[n] = true
			}
		}
	}
	for _, o := range overlays {
		if !used[o.Name] {
			out = append(out, o)
		}
	}
	return out
}

// handleTile serves one XYZ tile of an overlay.
//
// @Summary one XYZ tile of an overlay; {y} may carry the image extension
// @Tags imagery
// @Produce png,jpeg
// @Param name path string true "overlay name"
// @Param z path int true "zoom"
// @Param x path int true "tile column"
// @Param y path string true "tile row, e.g. 1234 or 1234.png"
// @Success 200
// @Success 204 "no tile there (outside the imagery)"
// @Router /mowglinext/tiles/{name}/{z}/{x}/{y} [get]
func (s *imageryStore) handleTile(c *gin.Context) {
	name := c.Param("name")
	z, x, y, ok := parseTileCoords(c.Param("z"), c.Param("x"), c.Param("y"))
	if !imageryNameRe.MatchString(name) || !ok {
		c.Status(http.StatusBadRequest)
		return
	}
	o, found := s.get(name)
	if !found || o.Status != "ready" || !imageryTileExts[o.TileExt] {
		c.Status(http.StatusNotFound)
		return
	}
	// Every path element is either the validated name or a re-formatted
	// integer, so the path cannot leave the overlay's tiles folder.
	p := tilePath(filepath.Join(s.root, name, "tiles"), z, x, y, o.TileExt)
	if st, err := os.Stat(p); err != nil || !st.Mode().IsRegular() {
		// Outside the imagery: an empty response renders as transparent and
		// keeps the browser console free of 404 noise.
		c.Header("Cache-Control", "public, max-age=86400")
		c.Status(http.StatusNoContent)
		return
	}
	// The web client adds ?v=<createdAt>, so a re-upload never hits a stale tile.
	c.Header("Cache-Control", "public, max-age=604800")
	c.File(p)
}

// parseTileCoords validates z/x/y (y may carry an extension) for the XYZ scheme.
func parseTileCoords(zs, xs, ys string) (z, x, y int, ok bool) {
	if i := strings.IndexByte(ys, '.'); i >= 0 {
		ys = ys[:i]
	}
	var err error
	if z, err = parseTileIndex(zs); err != nil || z > imageryMaxZoom {
		return 0, 0, 0, false
	}
	if x, err = parseTileIndex(xs); err != nil || x >= 1<<z {
		return 0, 0, 0, false
	}
	if y, err = parseTileIndex(ys); err != nil || y >= 1<<z {
		return 0, 0, 0, false
	}
	return z, x, y, true
}

// parseTileIndex accepts plain decimal digits only (no sign, no spaces).
func parseTileIndex(s string) (int, error) {
	if s == "" || len(s) > 10 || strings.TrimLeft(s, "0123456789") != "" {
		return 0, errors.New("not a tile index")
	}
	return strconv.Atoi(s)
}

func tilePath(dir string, z, x, y int, ext string) string {
	return filepath.Join(dir, strconv.Itoa(z), strconv.Itoa(x), strconv.Itoa(y)+ext)
}

func cleanImageryLabel(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > imageryMaxLabelLen {
		s = string(r[:imageryMaxLabelLen])
	}
	return s
}

// handleUpload streams the multipart upload to disk (no in-memory buffering)
// and starts the background conversion. Text fields (label, scheme) must come
// before the file part.
//
// @Summary upload a tile set (multipart: label, scheme auto|xyz|tms, then file)
// @Tags imagery
// @Accept mpfd
// @Produce json
// @Success 200 {object} imageryOverlay
// @Failure 400 {object} ErrorResponse
// @Failure 413 {object} ErrorResponse
// @Router /mowglinext/imagery [post]
func (s *imageryStore) handleUpload(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, imageryMaxUploadBytes+1<<20)
	mr, err := c.Request.MultipartReader()
	if err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: "expected multipart/form-data: " + err.Error()})
		return
	}
	fields := map[string]string{}
	var name, kind, srcPath string
	var size int64
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			s.abortUpload(name)
			c.JSON(http.StatusBadRequest, ErrorResponse{Error: "upload failed: " + err.Error()})
			return
		}
		switch part.FormName() {
		case "label", "scheme":
			b, _ := io.ReadAll(io.LimitReader(part, 1024))
			fields[part.FormName()] = string(b)
			continue
		case "file":
			if srcPath != "" {
				continue // one file per overlay
			}
		default:
			continue // unknown parts are skipped, never buffered
		}
		var ext string
		kind, ext, err = imageryKindForFile(part.FileName())
		if err != nil {
			c.JSON(http.StatusBadRequest, ErrorResponse{Error: err.Error()})
			return
		}
		label := cleanImageryLabel(fields["label"])
		if label == "" {
			base := filepath.Base(part.FileName())
			label = cleanImageryLabel(strings.TrimSuffix(base, filepath.Ext(base)))
		}
		fields["label"] = label
		s.mu.Lock()
		name = s.uniqueNameLocked(label)
		dir := filepath.Join(s.root, name)
		err = os.MkdirAll(dir, 0o755)
		s.mu.Unlock()
		if err != nil {
			c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
			return
		}
		srcPath = filepath.Join(dir, "source"+ext)
		size, err = writeUpload(srcPath, part)
		if err != nil {
			s.abortUpload(name)
			c.JSON(http.StatusBadRequest, ErrorResponse{Error: "upload failed: " + err.Error()})
			return
		}
		if size > imageryMaxUploadBytes {
			s.abortUpload(name)
			c.JSON(http.StatusRequestEntityTooLarge, ErrorResponse{Error: fmt.Sprintf("file exceeds %d MB", imageryMaxUploadBytes>>20)})
			return
		}
	}
	if srcPath == "" {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: "no file in upload"})
		return
	}

	o := &imageryOverlay{
		Name: name, Label: fields["label"], Kind: kind, Status: "processing",
		Opacity: 1, Visible: true, SizeBytes: size, CreatedAt: time.Now().Unix(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.overlays = append(s.overlays, o)
	s.cancels[name] = cancel
	err = s.saveLocked()
	snapshot := *o
	s.mu.Unlock()
	if err != nil {
		cancel()
		s.mu.Lock()
		if i, _ := s.findLocked(name); i >= 0 {
			s.overlays = append(s.overlays[:i], s.overlays[i+1:]...)
		}
		delete(s.cancels, name)
		s.mu.Unlock()
		s.abortUpload(name)
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	go s.process(ctx, name, kind, srcPath, fields["scheme"])
	c.JSON(http.StatusOK, snapshot)
}

// writeUpload copies at most imageryMaxUploadBytes+1 bytes of r to path.
func writeUpload(path string, r io.Reader) (int64, error) {
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, io.LimitReader(r, imageryMaxUploadBytes+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return n, err
}

func (s *imageryStore) abortUpload(name string) {
	if name != "" {
		_ = os.RemoveAll(filepath.Join(s.root, name))
	}
}

// process converts an upload in the background and records the outcome.
func (s *imageryStore) process(ctx context.Context, name, kind, srcPath, scheme string) {
	dir := filepath.Dir(srcPath)
	progress := func(p float64) { s.setProgress(name, p) }
	var res imageryResult
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("conversion crashed: %v", r)
			}
		}()
		switch kind {
		case "xyz":
			res, err = processXYZZip(ctx, srcPath, filepath.Join(dir, "tiles"), scheme, progress)
		default:
			err = fmt.Errorf("unknown kind %q", kind)
		}
	}()
	// The upload is only an intermediate: the tiles are the product.
	_ = os.Remove(srcPath)

	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cancels, name)
	if ctx.Err() != nil {
		// Deleted while processing: the delete handler left the folder to us.
		_ = os.RemoveAll(dir)
		return
	}
	_, o := s.findLocked(name)
	if o == nil {
		return
	}
	if err != nil {
		log.Printf("imagery %s: %v", name, err)
		o.Status = "error"
		o.Error = err.Error()
		_ = os.RemoveAll(filepath.Join(dir, "tiles"))
	} else {
		o.Status = "ready"
		o.Progress = 1
		o.MinZoom, o.MaxZoom = res.minZoom, res.maxZoom
		o.Bounds = res.bounds
		o.TileExt = res.tileExt
	}
	if err := s.saveLocked(); err != nil {
		log.Printf("imagery: save overlays.json: %v", err)
	}
}

type imageryResult struct {
	minZoom, maxZoom int
	bounds           *[4]float64
	tileExt          string
}

// ---------------------------------------------------------------------------
// Tile sets
// ---------------------------------------------------------------------------

// tileFracToLonLat returns the lon/lat of fractional XYZ tile coordinates.
func tileFracToLonLat(x, y float64, z int) (lon, lat float64) {
	n := math.Exp2(float64(z))
	lon = x/n*360 - 180
	lat = math.Atan(math.Sinh(math.Pi*(1-2*y/n))) * 180 / math.Pi
	return lon, lat
}

// tmsToXYZ flips a TMS tile row into the XYZ scheme.
func tmsToXYZ(z, y int) int { return (1 << uint(z)) - 1 - y }

// tileSetStats collects the zoom range and footprint of a tile set while it
// is unpacked.
type tileSetStats struct {
	minZ, maxZ int
	ext        string
	xr         map[int][4]int // z -> x0,y0,x1,y1
	count      int
	bytes      int64
}

func newTileSetStats() *tileSetStats {
	return &tileSetStats{minZ: imageryMaxZoom + 1, maxZ: -1, xr: map[int][4]int{}}
}

func (t *tileSetStats) add(z, x, y int, n int64) error {
	t.bytes += n
	if t.bytes > imageryMaxUnpackedBytes {
		return fmt.Errorf("tile set unpacks to more than %d MB", imageryMaxUnpackedBytes>>20)
	}
	t.count++
	t.minZ, t.maxZ = min(t.minZ, z), max(t.maxZ, z)
	r, ok := t.xr[z]
	if !ok {
		r = [4]int{x, y, x, y}
	}
	r[0], r[1] = min(r[0], x), min(r[1], y)
	r[2], r[3] = max(r[2], x), max(r[3], y)
	t.xr[z] = r
	return nil
}

func (t *tileSetStats) result() (imageryResult, error) {
	if t.count == 0 {
		return imageryResult{}, errors.New("no z/x/y tiles found")
	}
	// Union over every zoom: an overview level may reach further than the
	// detailed one, and Mapbox requests no tile outside these bounds.
	b := [4]float64{180, 90, -180, -90}
	for z, r := range t.xr {
		w, n := tileFracToLonLat(float64(r[0]), float64(r[1]), z)
		e, s := tileFracToLonLat(float64(r[2]+1), float64(r[3]+1), z)
		b = [4]float64{min(b[0], w), min(b[1], s), max(b[2], e), max(b[3], n)}
	}
	return imageryResult{minZoom: t.minZ, maxZoom: t.maxZ, bounds: &b, tileExt: t.ext}, nil
}

// validTile reports whether z/x/y is a real tile of the XYZ/TMS grid.
func validTile(z, x, y int) bool {
	return z >= 0 && z <= imageryMaxZoom && x >= 0 && y >= 0 && x < 1<<z && y < 1<<z
}

var xyzEntryRe = regexp.MustCompile(`(?:^|/)(\d{1,2})/(\d{1,10})/(\d{1,10})\.(png|jpg|jpeg|webp)$`)

func normTileExt(ext string) string {
	ext = "." + strings.ToLower(strings.TrimPrefix(ext, "."))
	if ext == ".jpeg" {
		return ".jpg"
	}
	return ext
}

// processXYZZip unpacks the <z>/<x>/<y>.<ext> entries of a zip into tileDir.
// Entry names are only matched, never used as paths, so a crafted zip cannot
// write outside tileDir. scheme is "xyz", "tms" or "auto" (TMS when the zip
// carries gdal2tiles' tilemapresource.xml).
func processXYZZip(ctx context.Context, srcPath, tileDir, scheme string, progress func(float64)) (imageryResult, error) {
	zr, err := zip.OpenReader(srcPath)
	if err != nil {
		return imageryResult{}, fmt.Errorf("open zip: %w", err)
	}
	defer zr.Close()
	tms := scheme == "tms"
	if scheme != "xyz" && scheme != "tms" {
		for _, f := range zr.File {
			if strings.HasSuffix(strings.ToLower(f.Name), "tilemapresource.xml") {
				tms = true // gdal2tiles default (non --xyz) output
				break
			}
		}
	}
	st := newTileSetStats()
	for i, f := range zr.File {
		if ctx.Err() != nil {
			return imageryResult{}, ctx.Err()
		}
		m := xyzEntryRe.FindStringSubmatch(strings.ReplaceAll(f.Name, "\\", "/"))
		if m == nil || f.FileInfo().IsDir() {
			continue
		}
		z, _ := strconv.Atoi(m[1])
		x, _ := strconv.Atoi(m[2])
		y, _ := strconv.Atoi(m[3])
		if !validTile(z, x, y) {
			continue
		}
		ext := normTileExt(m[4])
		if st.ext == "" {
			st.ext = ext
		} else if st.ext != ext {
			continue // mixed formats: keep the first one
		}
		if tms {
			y = tmsToXYZ(z, y)
		}
		n, err := extractZipEntry(f, tilePath(tileDir, z, x, y, ext))
		if err != nil {
			return imageryResult{}, err
		}
		if err := st.add(z, x, y, n); err != nil {
			return imageryResult{}, err
		}
		if i%64 == 0 {
			progress(float64(i) / float64(len(zr.File)))
		}
	}
	res, err := st.result()
	if err != nil {
		return res, fmt.Errorf("%w in the zip (expected <z>/<x>/<y>.png, e.g. from `gdal2tiles.py --xyz`)", err)
	}
	return res, nil
}

func extractZipEntry(f *zip.File, dst string) (int64, error) {
	if f.UncompressedSize64 > imageryMaxTileBytes {
		return 0, fmt.Errorf("tile %s is implausibly large", f.Name)
	}
	rc, err := f.Open()
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	return writeTile(dst, rc)
}

// writeTile writes one tile, refusing tiles above imageryMaxTileBytes.
func writeTile(dst string, r io.Reader) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, err
	}
	out, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, io.LimitReader(r, imageryMaxTileBytes+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > imageryMaxTileBytes {
		err = fmt.Errorf("tile %s is implausibly large", filepath.Base(dst))
	}
	return n, err
}
