import {useEffect} from "react";
import {Layer, Source, useMap} from "react-map-gl/mapbox";
import {httpBase} from "../../../utils/apiHost.ts";
import {drawableOverlays, imageryLayerId, imageryTileUrl, type ImageryOverlay} from "../../../utils/imagery.ts";

/**
 * Custom map tile layers, kept directly above the base style (and the optional
 * tile-server layer) and below every robot/map layer, stacked in list order
 * (index 0 = bottom). Render it right after the "custom-layer" raster.
 */
export function ImageryLayers({overlays}: {overlays: ImageryOverlay[]}) {
    const {current: map} = useMap();
    const list = drawableOverlays(overlays);
    const order = list.map((o) => imageryLayerId(o.name)).join("|");

    // A layer added later (a new overlay, a re-enabled one, a style switch)
    // lands on top of the stack, above the app layers: put ours back in order,
    // directly under the first app layer.
    useEffect(() => {
        if (!map || !order) return;
        const m = map.getMap();
        const ids = order.split("|");
        let frame = 0;
        const fix = () => {
            if (frame) return;
            frame = requestAnimationFrame(() => {
                frame = 0;
                try {
                    const layers = (m.getStyle()?.layers ?? []).map((l) => l.id);
                    const present = ids.filter((id) => layers.includes(id));
                    if (!present.length) return;
                    const anchor = firstAppLayer(layers, present);
                    const firstIdx = layers.indexOf(present[0]);
                    const inOrder = layers.slice(firstIdx, firstIdx + present.length).join("|") === present.join("|");
                    const anchorOk = !anchor || layers.indexOf(anchor) === firstIdx + present.length;
                    if (inOrder && anchorOk) return;
                    for (const id of present) m.moveLayer(id, anchor);
                } catch { /* style not loaded yet: the next styledata event retries */ }
            });
        };
        fix();
        m.on("styledata", fix);
        return () => {
            m.off("styledata", fix);
            if (frame) cancelAnimationFrame(frame);
        };
    }, [map, order]);

    const base = httpBase();
    return (
        <>
            {list.map((o) => (
                <Source key={o.name} id={`imagery-${o.name}`} type="raster" tileSize={256}
                        tiles={[imageryTileUrl(o, base)]}
                        minzoom={o.minZoom ?? 0} maxzoom={o.maxZoom ?? 22}
                        bounds={o.bounds}>
                    <Layer id={imageryLayerId(o.name)} type="raster"
                           paint={{"raster-opacity": o.opacity, "raster-fade-duration": 0}}/>
                </Source>
            ))}
        </>
    );
}

/**
 * The layer ours must sit under: the first layer after the tile-server raster
 * ("custom-layer") that is not ours, else the robot label layer ("mower"),
 * which is the first app layer the Map page adds. undefined = top.
 */
function firstAppLayer(layers: string[], ours: string[]): string | undefined {
    const after = layers.indexOf("custom-layer");
    if (after < 0) return layers.includes("mower") ? "mower" : undefined;
    return layers.slice(after + 1).find((id) => !ours.includes(id));
}
