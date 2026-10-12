import {afterEach, describe, expect, it, vi} from "vitest";
import {act, renderHook, waitFor} from "@testing-library/react";
import {useImagery} from "./useImagery.ts";

const ov = (name: string, extra: object = {}) => ({
    name, label: name, kind: "xyz", status: "ready", progress: 1, opacity: 1, visible: true, sizeBytes: 1, createdAt: 1, ...extra,
});

function mockFetch(handler: (url: string, init?: RequestInit) => {status?: number; body: unknown}) {
    const fn = vi.fn((url: string, init?: RequestInit) => {
        const {status = 200, body} = handler(url, init);
        return Promise.resolve(new Response(typeof body === "string" ? body : JSON.stringify(body), {status}));
    });
    vi.stubGlobal("fetch", fn);
    return fn;
}

afterEach(() => { vi.unstubAllGlobals(); });

describe("useImagery", () => {
    it("loads the list, patches opacity optimistically, reorders and deletes", async () => {
        const fetchMock = mockFetch((url, init) => {
            if (url.endsWith("/imagery") && !init?.method) return {body: {overlays: [ov("a"), ov("b")]}};
            if (init?.method === "PATCH") return {body: {...ov("a"), ...JSON.parse(init.body as string)}};
            if (url.endsWith("/imagery-order")) return {body: {overlays: [ov("b"), ov("a")]}};
            return {body: {}};
        });
        const {result} = renderHook(() => useImagery());
        await waitFor(() => expect(result.current.overlays).toHaveLength(2));

        await act(async () => { await result.current.update("a", {opacity: 0.4, visible: false}); });
        expect(result.current.overlays[0]).toMatchObject({name: "a", opacity: 0.4, visible: false});
        const patch = fetchMock.mock.calls.find(([, i]) => i?.method === "PATCH")!;
        expect(patch[0]).toContain("/api/mowglinext/imagery/a");
        expect(JSON.parse(patch[1]!.body as string)).toEqual({opacity: 0.4, visible: false});

        await act(async () => { await result.current.reorder(["b", "a"]); });
        expect(result.current.overlays.map((o) => o.name)).toEqual(["b", "a"]);

        await act(async () => { await result.current.remove("b"); });
        expect(result.current.overlays.map((o) => o.name)).toEqual(["a"]);
    });

    it("surfaces the server error and reloads the list when a patch fails", async () => {
        let patched = false;
        mockFetch((_url, init) => {
            if (init?.method === "PATCH") { patched = true; return {status: 404, body: {error: "no such overlay"}}; }
            return {body: {overlays: patched ? [] : [ov("a")]}};
        });
        const {result} = renderHook(() => useImagery());
        await waitFor(() => expect(result.current.overlays).toHaveLength(1));
        await act(async () => {
            await expect(result.current.update("a", {visible: false})).rejects.toThrow("no such overlay");
        });
        await waitFor(() => expect(result.current.overlays).toHaveLength(0));
    });

    it("reports a non-JSON error body by its status", async () => {
        mockFetch((_url, init) => init?.method === "DELETE" ? {status: 502, body: "<html>bad gateway</html>"} : {body: {overlays: [ov("a")]}});
        const {result} = renderHook(() => useImagery());
        await waitFor(() => expect(result.current.overlays).toHaveLength(1));
        await act(async () => {
            await expect(result.current.remove("a")).rejects.toThrow("HTTP 502");
        });
    });
});
