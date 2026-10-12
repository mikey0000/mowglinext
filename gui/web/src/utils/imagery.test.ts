import {describe, expect, it} from "vitest";
import {drawableOverlays, imageryTileUrl, moveOverlay, validateUpload, type ImageryOverlay} from "./imagery.ts";

const overlay = (o: Partial<ImageryOverlay>): ImageryOverlay => ({
    name: "a", label: "a", kind: "xyz", status: "ready", progress: 1, opacity: 1, visible: true, sizeBytes: 1, createdAt: 1, ...o,
});

describe("custom map tile helpers", () => {
    it("moveOverlay swaps neighbours and clamps at the ends", () => {
        expect(moveOverlay(["a", "b", "c"], "a", "up")).toEqual(["b", "a", "c"]);
        expect(moveOverlay(["a", "b", "c"], "c", "up")).toEqual(["a", "b", "c"]);
        expect(moveOverlay(["a", "b", "c"], "b", "down")).toEqual(["b", "a", "c"]);
        expect(moveOverlay(["a", "b", "c"], "a", "down")).toEqual(["a", "b", "c"]);
        expect(moveOverlay(["a"], "zz", "up")).toEqual(["a"]);
    });

    it("drawableOverlays keeps ready and visible overlays only", () => {
        const list = [
            overlay({name: "t"}),
            overlay({name: "hidden", visible: false}),
            overlay({name: "busy", status: "processing"}),
            overlay({name: "broken", status: "error"}),
        ];
        expect(drawableOverlays(list).map((o) => o.name)).toEqual(["t"]);
    });

    it("tile URL template carries the cache-busting version and extension", () => {
        expect(imageryTileUrl({name: "drone june", createdAt: 42, tileExt: ".jpg"}))
            .toBe("/api/mowglinext/tiles/drone%20june/{z}/{x}/{y}.jpg?v=42");
        expect(imageryTileUrl({name: "a", createdAt: 1}, "http://robot")).toBe("http://robot/api/mowglinext/tiles/a/{z}/{x}/{y}.png?v=1");
    });

    it("validateUpload checks type and the 300 MB limit", () => {
        expect(validateUpload({name: "tiles.ZIP", size: 10})).toBeNull();
        expect(validateUpload({name: "garden.mbtiles", size: 10})).toBeNull();
        expect(validateUpload({name: "o.exe", size: 10})).toBe("type");
        expect(validateUpload({name: "zip", size: 10})).toBe("type");
        expect(validateUpload({name: "o.zip", size: 301 * 1024 * 1024})).toBe("size");
    });
});
