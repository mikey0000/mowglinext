import {useCallback, useEffect, useRef, useState} from "react";
import {httpBase} from "../utils/apiHost.ts";
import type {ImageryOverlay} from "../utils/imagery.ts";

const BASE = "/api/mowglinext";

function errorMessage(text: string, status: number): string {
    try {
        const body = JSON.parse(text) as {error?: string};
        if (body.error) return body.error;
    } catch { /* not JSON (e.g. a proxy error page) */ }
    return `HTTP ${status}`;
}

async function jsonFetch<T>(path: string, init?: RequestInit): Promise<T> {
    const res = await fetch(`${httpBase()}${BASE}${path}`, {
        ...init,
        headers: init?.body ? {"Content-Type": "application/json"} : undefined,
    });
    const text = await res.text();
    if (!res.ok) throw new Error(errorMessage(text, res.status));
    return (text ? JSON.parse(text) : {}) as T;
}

async function fetchList(): Promise<ImageryOverlay[]> {
    const r = await jsonFetch<{overlays?: ImageryOverlay[]}>("/imagery");
    return r.overlays ?? [];
}

export interface ImageryPatch {
    label?: string;
    opacity?: number;
    visible?: boolean;
}

export interface UploadState {
    fileName: string;
    /** 0..1 of the HTTP transfer. */
    progress: number;
}

/**
 * Custom map tile overlays (bottom-most first). Polls while any overlay is
 * still being unpacked on the robot.
 */
export function useImagery(enabled = true) {
    const [overlays, setOverlays] = useState<ImageryOverlay[]>([]);
    const [upload, setUpload] = useState<UploadState | null>(null);
    const xhrRef = useRef<XMLHttpRequest | null>(null);

    const refresh = useCallback(() => fetchList().then(setOverlays, () => {
        /* robot unreachable: keep the last list */
    }), []);

    useEffect(() => {
        if (!enabled) return;
        let live = true;
        fetchList().then((l) => { if (live) setOverlays(l); }, () => { /* keep the last list */ });
        return () => { live = false; };
    }, [enabled]);

    const processing = overlays.some((o) => o.status === "processing");
    useEffect(() => {
        if (!enabled || !processing) return;
        const id = setInterval(() => { void refresh(); }, 1500);
        return () => clearInterval(id);
    }, [enabled, processing, refresh]);

    /** Local-only change (e.g. while a slider is dragged); persist with update(). */
    const preview = useCallback((name: string, patch: ImageryPatch) => {
        setOverlays((l) => l.map((o) => o.name === name ? {...o, ...patch} : o));
    }, []);

    const update = useCallback(async (name: string, patch: ImageryPatch) => {
        // Optimistic so sliders feel immediate.
        setOverlays((l) => l.map((o) => o.name === name ? {...o, ...patch} : o));
        try {
            const o = await jsonFetch<ImageryOverlay>(`/imagery/${encodeURIComponent(name)}`, {method: "PATCH", body: JSON.stringify(patch)});
            setOverlays((l) => l.map((x) => x.name === name ? o : x));
            return o;
        } catch (e) {
            void refresh();
            throw e;
        }
    }, [refresh]);

    const reorder = useCallback(async (names: string[]) => {
        setOverlays((l) => names.map((n) => l.find((o) => o.name === n)).filter((o): o is ImageryOverlay => !!o));
        try {
            const r = await jsonFetch<{overlays?: ImageryOverlay[]}>("/imagery-order", {method: "POST", body: JSON.stringify({names})});
            setOverlays(r.overlays ?? []);
        } catch (e) {
            void refresh();
            throw e;
        }
    }, [refresh]);

    const remove = useCallback(async (name: string) => {
        await jsonFetch(`/imagery/${encodeURIComponent(name)}`, {method: "DELETE"});
        setOverlays((l) => l.filter((o) => o.name !== name));
    }, []);

    // XMLHttpRequest rather than fetch: fetch cannot report upload progress.
    const uploadFile = useCallback((file: File, opts: {label?: string; scheme?: string} = {}) => {
        return new Promise<ImageryOverlay>((resolve, reject) => {
            const fd = new FormData();
            if (opts.label) fd.append("label", opts.label);
            if (opts.scheme) fd.append("scheme", opts.scheme);
            fd.append("file", file); // last: the server reads the fields before the file
            const xhr = new XMLHttpRequest();
            xhrRef.current = xhr;
            const done = () => { xhrRef.current = null; setUpload(null); };
            xhr.open("POST", `${httpBase()}${BASE}/imagery`);
            setUpload({fileName: file.name, progress: 0});
            xhr.upload.onprogress = (ev) => {
                if (ev.lengthComputable) setUpload({fileName: file.name, progress: ev.loaded / ev.total});
            };
            xhr.onload = () => {
                done();
                if (xhr.status < 200 || xhr.status >= 300) {
                    reject(new Error(errorMessage(xhr.responseText, xhr.status)));
                    return;
                }
                try {
                    const o = JSON.parse(xhr.responseText) as ImageryOverlay;
                    setOverlays((l) => [...l, o]);
                    resolve(o);
                } catch {
                    reject(new Error("invalid response"));
                }
            };
            xhr.onerror = () => { done(); reject(new Error("network error")); };
            xhr.onabort = () => { done(); reject(new Error("cancelled")); };
            xhr.send(fd);
        });
    }, []);

    const cancelUpload = useCallback(() => { xhrRef.current?.abort(); }, []);

    return {overlays, refresh, preview, update, reorder, remove, uploadFile, upload, cancelUpload};
}

export type ImageryApi = ReturnType<typeof useImagery>;
