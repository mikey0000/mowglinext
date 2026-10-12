import {useState} from "react";
import {App, Button, Popconfirm, Progress, Select, Slider, Switch, Tooltip, Upload} from "antd";
import {ArrowDownOutlined, ArrowUpOutlined, DeleteOutlined, InboxOutlined, ZoomInOutlined} from "@ant-design/icons";
import {useTranslation} from "react-i18next";
import {useThemeMode} from "../../../theme/ThemeContext.tsx";
import type {ImageryApi} from "../../../hooks/useImagery.ts";
import {IMAGERY_ACCEPT, IMAGERY_MAX_UPLOAD_BYTES, moveOverlay, validateUpload, type ImageryOverlay} from "../../../utils/imagery.ts";

interface Props {
    imagery: ImageryApi;
    onZoomTo: (o: ImageryOverlay) => void;
}

const MAX_MB = IMAGERY_MAX_UPLOAD_BYTES / 1024 / 1024;

/** Custom map tiles: upload plus per-overlay visibility, opacity, order, delete. */
export function ImageryPanel({imagery, onZoomTo}: Props) {
    const {t} = useTranslation();
    const {colors} = useThemeMode();
    const {notification} = App.useApp();
    const {overlays, preview, update, reorder, remove, uploadFile, upload, cancelUpload} = imagery;
    const [scheme, setScheme] = useState("auto");

    const doUpload = async (file: File) => {
        const bad = validateUpload(file);
        if (bad) {
            notification.error({message: bad === "size" ? t("imagery.tooLarge", {mb: MAX_MB}) : t("imagery.badType")});
            return;
        }
        try {
            const o = await uploadFile(file, {scheme});
            notification.success({message: t("imagery.uploaded", {label: o.label}), description: t("imagery.processingHint")});
        } catch (e) {
            const msg = e instanceof Error ? e.message : String(e);
            if (msg !== "cancelled") notification.error({message: t("imagery.uploadFailed"), description: msg});
        }
    };

    const names = overlays.map((o) => o.name);
    const onError = (e: unknown) => notification.error({message: t("imagery.saveFailed"), description: e instanceof Error ? e.message : String(e)});

    return (
        <div style={{fontSize: 12}}>
            {upload ? (
                <div style={{marginBottom: 8}}>
                    <div style={{overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap"}}>{t("imagery.uploading", {file: upload.fileName})}</div>
                    <div style={{display: "flex", alignItems: "center", gap: 6}}>
                        <Progress percent={Math.round(upload.progress * 100)} size="small" style={{flex: 1, margin: 0}}/>
                        <Button size="small" type="link" onClick={cancelUpload}>{t("imagery.cancel")}</Button>
                    </div>
                </div>
            ) : (
                <Upload.Dragger accept={IMAGERY_ACCEPT} multiple={false} showUploadList={false}
                                beforeUpload={(file) => { void doUpload(file); return false; }}
                                style={{padding: 0, marginBottom: 6}}>
                    <div style={{padding: "4px 6px"}}>
                        <InboxOutlined style={{fontSize: 20, color: colors.primary}}/>
                        <div>{t("imagery.drop")}</div>
                        <div style={{color: colors.textSecondary, fontSize: 11}}>{t("imagery.dropHint", {mb: MAX_MB})}</div>
                    </div>
                </Upload.Dragger>
            )}
            <div style={{display: "flex", alignItems: "center", gap: 6, fontSize: 11, color: colors.textSecondary, marginBottom: 6}}>
                <span>{t("imagery.zipScheme")}</span>
                <Select size="small" value={scheme} onChange={setScheme} style={{flex: 1}}
                        options={[{value: "auto", label: t("imagery.schemeAuto")}, {value: "xyz", label: "XYZ"}, {value: "tms", label: "TMS"}]}/>
            </div>
            {overlays.length === 0 && <div style={{color: colors.textSecondary}}>{t("imagery.empty")}</div>}
            {/* Top of the stack first. */}
            {[...overlays].reverse().map((o) => {
                const idx = names.indexOf(o.name);
                const ready = o.status === "ready";
                return (
                    <div key={o.name} style={{borderTop: `1px solid ${colors.borderSubtle}`, padding: "6px 0"}}>
                        <div style={{display: "flex", alignItems: "center", gap: 6}}>
                            <Switch size="small" checked={o.visible} disabled={!ready} aria-label={t("imagery.visible")}
                                    onChange={(v) => { update(o.name, {visible: v}).catch(onError); }}/>
                            <span style={{flex: 1, fontWeight: 600, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap"}} title={o.label}>{o.label}</span>
                        </div>
                        {o.status === "processing" && <Progress percent={Math.round(o.progress * 100)} size="small" status="active" format={() => t("imagery.processing")}/>}
                        {o.status === "error" && <div style={{color: colors.danger, wordBreak: "break-word"}}>{o.error}</div>}
                        {ready && (
                            <Slider min={0} max={1} step={0.05} value={o.opacity} style={{margin: "4px 0"}}
                                    aria-label={t("imagery.opacity")}
                                    tooltip={{formatter: (v) => `${Math.round((v ?? 0) * 100)}%`}}
                                    onChange={(v) => preview(o.name, {opacity: v})}
                                    onChangeComplete={(v) => { update(o.name, {opacity: v}).catch(onError); }}/>
                        )}
                        <div style={{display: "flex", gap: 2, justifyContent: "flex-end"}}>
                            {ready && o.bounds && (
                                <Tooltip title={t("imagery.zoomTo")}>
                                    <Button size="small" type="text" icon={<ZoomInOutlined/>} aria-label={t("imagery.zoomTo")} onClick={() => onZoomTo(o)}/>
                                </Tooltip>
                            )}
                            <Tooltip title={t("imagery.moveUp")}>
                                <Button size="small" type="text" icon={<ArrowUpOutlined/>} aria-label={t("imagery.moveUp")} disabled={idx >= names.length - 1}
                                        onClick={() => { reorder(moveOverlay(names, o.name, "up")).catch(onError); }}/>
                            </Tooltip>
                            <Tooltip title={t("imagery.moveDown")}>
                                <Button size="small" type="text" icon={<ArrowDownOutlined/>} aria-label={t("imagery.moveDown")} disabled={idx <= 0}
                                        onClick={() => { reorder(moveOverlay(names, o.name, "down")).catch(onError); }}/>
                            </Tooltip>
                            <Popconfirm title={t("imagery.deleteConfirm", {label: o.label})} okText={t("imagery.delete")} okButtonProps={{danger: true}}
                                        onConfirm={() => { remove(o.name).catch(onError); }}>
                                <Button size="small" type="text" danger icon={<DeleteOutlined/>} aria-label={t("imagery.delete")}/>
                            </Popconfirm>
                        </div>
                    </div>
                );
            })}
        </div>
    );
}
