/**
 * Custom map tiles: operator-supplied XYZ tile sets (drone orthomosaics etc.)
 * drawn as extra raster layers on the Map page. Pure helpers shared by the
 * layers, the panel and the hook — the backend is gui/pkg/api/imagery.go.
 */

export type ImageryKind = "xyz" | "mbtiles";

export interface ImageryOverlay {
    name: string;
    label: string;
    kind: ImageryKind;
    status: "processing" | "ready" | "error";
    error?: string;
    /** 0..1 while the robot unpacks the upload. */
    progress: number;
    opacity: number;
    visible: boolean;
    minZoom?: number;
    maxZoom?: number;
    /** west, south, east, north */
    bounds?: [number, number, number, number];
    tileExt?: string;
    sizeBytes: number;
    createdAt: number;
}

export const IMAGERY_MAX_UPLOAD_BYTES = 300 * 1024 * 1024;
export const IMAGERY_ACCEPT = ".zip,.mbtiles";

/** Tile URL template for Mapbox; createdAt busts the browser cache on re-upload. */
export function imageryTileUrl(o: Pick<ImageryOverlay, "name" | "createdAt" | "tileExt">, base = ""): string {
    const ext = o.tileExt || ".png";
    return `${base}/api/mowglinext/tiles/${encodeURIComponent(o.name)}/{z}/{x}/{y}${ext}?v=${o.createdAt}`;
}

/** Overlays the map should draw. */
export function drawableOverlays(list: ImageryOverlay[]): ImageryOverlay[] {
    return list.filter((o) => o.status === "ready" && o.visible);
}

/** Move an overlay one step up (towards the top of the stack) or down. */
export function moveOverlay(names: string[], name: string, dir: "up" | "down"): string[] {
    const i = names.indexOf(name);
    if (i < 0) return names;
    const j = dir === "up" ? i + 1 : i - 1; // index 0 = bottom-most
    if (j < 0 || j >= names.length) return names;
    const out = names.slice();
    [out[i], out[j]] = [out[j], out[i]];
    return out;
}

export function imageryLayerId(name: string): string {
    return `imagery-${name}-layer`;
}

/** Client-side pre-check of an upload: null when fine, else the reason. */
export function validateUpload(file: {name: string; size: number}): "type" | "size" | null {
    const dot = file.name.lastIndexOf(".");
    const ext = dot >= 0 ? file.name.slice(dot).toLowerCase() : "";
    if (!IMAGERY_ACCEPT.split(",").includes(ext)) return "type";
    if (file.size > IMAGERY_MAX_UPLOAD_BYTES) return "size";
    return null;
}
