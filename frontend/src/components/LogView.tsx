import { useEffect, useMemo, useRef, useState } from "react";
import { Button, Segmented, Select, Tooltip } from "antd";
import {
  ClearOutlined,
  VerticalAlignBottomOutlined,
  PauseCircleOutlined,
  PlayCircleOutlined,
} from "@ant-design/icons";
import { api } from "../bindings";
import { useAppStore } from "../store";
import type { LogEntry } from "../types";
import { hostDisplayName } from "../types";

const LEVEL_CLASS: Record<number, string> = {
  0: "log-info",
  1: "log-success",
  2: "log-warn",
  3: "log-error",
};

type FilterKey = "all" | "info" | "success" | "warn" | "error";

const FILTER_LEVELS: Record<FilterKey, number[] | null> = {
  all: null,
  info: [0],
  success: [1],
  warn: [2],
  error: [3],
};

// 无主机前缀的日志（应用级消息）归入"系统消息"。
const SYSTEM_FILTER = "__system__";

// 同步器日志统一以"[主机标签] "开头，据此按主机筛选。
function logHostLabel(text: string): string | null {
  const m = /^\[([^\]]+)\]/.exec(text);
  return m ? m[1] : null;
}

export default function LogView() {
  const logs = useAppStore((s) => s.logs);
  const hosts = useAppStore((s) => s.hosts);
  const theme = useAppStore((s) => s.theme);
  const clearLogs = useAppStore((s) => s.clearLogs);
  const [filter, setFilter] = useState<FilterKey>("all");
  const [hostFilter, setHostFilter] = useState<string>("all");
  const [autoScroll, setAutoScroll] = useState(true);
  const boxRef = useRef<HTMLDivElement>(null);

  const hostLabelById = useMemo(() => {
    const m = new Map<string, string>();
    hosts.forEach((h) => {
      if (h.id) m.set(h.id, hostDisplayName(h));
    });
    return m;
  }, [hosts]);

  const hostOptions = useMemo(
    () => [
      { value: "all", label: "全部主机" },
      ...[...hostLabelById.entries()].map(([id, label]) => ({ value: id, label })),
      { value: SYSTEM_FILTER, label: "系统消息" },
    ],
    [hostLabelById],
  );

  const effectiveFilter =
    hostFilter !== "all" && hostFilter !== SYSTEM_FILTER && !hostLabelById.has(hostFilter)
      ? "all"
      : hostFilter;

  const visible = useMemo(() => {
    const levels = FILTER_LEVELS[filter];
    const knownLabels = new Set(hostLabelById.values());
    return logs.filter((l: LogEntry) => {
      if (levels && !levels.includes(l.level)) return false;
      if (effectiveFilter === "all") return true;
      const label = logHostLabel(l.text);
      if (effectiveFilter === SYSTEM_FILTER) {
        return label === null || !knownLabels.has(label);
      }
      return label === hostLabelById.get(effectiveFilter);
    });
  }, [logs, filter, effectiveFilter, hostLabelById]);

  useEffect(() => {
    if (autoScroll && boxRef.current) {
      boxRef.current.scrollTop = boxRef.current.scrollHeight;
    }
  }, [visible, autoScroll]);

  return (
    <div style={{ display: "flex", flexDirection: "column", flex: 1, minHeight: 0 }}>
      <div className="log-toolbar">
        <Select
          size="small"
          value={effectiveFilter}
          onChange={setHostFilter}
          options={hostOptions}
          style={{ minWidth: 150 }}
        />
        <Segmented
          value={filter}
          onChange={(v) => setFilter(v as FilterKey)}
          options={[
            { value: "all", label: "全部" },
            { value: "info", label: "信息" },
            { value: "success", label: "成功" },
            { value: "warn", label: "警告" },
            { value: "error", label: "错误" },
          ]}
        />
        <div style={{ flex: 1 }} />
        <Tooltip title={autoScroll ? "暂停自动滚动" : "恢复自动滚动"}>
          <Button
            type={autoScroll ? "primary" : "default"}
            ghost={autoScroll}
            size="small"
            icon={autoScroll ? <PauseCircleOutlined /> : <PlayCircleOutlined />}
            onClick={() => setAutoScroll(!autoScroll)}
          />
        </Tooltip>
        <Tooltip title="跳到底部">
          <Button
            size="small"
            icon={<VerticalAlignBottomOutlined />}
            onClick={() => {
              if (boxRef.current) boxRef.current.scrollTop = boxRef.current.scrollHeight;
            }}
          />
        </Tooltip>
        <Tooltip title="清空日志">
          <Button
            size="small"
            icon={<ClearOutlined />}
            onClick={() => {
              clearLogs();
              api.clearLog().catch(() => undefined);
            }}
          />
        </Tooltip>
      </div>
      <div ref={boxRef} className={`log-view${theme === "dark" ? " dark" : ""}`}>
        {visible.length === 0 ? (
          <span style={{ opacity: 0.5 }}>暂无日志</span>
        ) : (
          visible.map((entry: LogEntry, i: number) => (
            <span key={i} className={`log-line ${LEVEL_CLASS[entry.level] ?? "log-info"}`}>
              {entry.text}
            </span>
          ))
        )}
      </div>
    </div>
  );
}
