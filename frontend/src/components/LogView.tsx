import { useEffect, useMemo, useRef, useState } from "react";
import { Button, Segmented, Tooltip } from "antd";
import {
  ClearOutlined,
  VerticalAlignBottomOutlined,
  PauseCircleOutlined,
  PlayCircleOutlined,
} from "@ant-design/icons";
import { api } from "../bindings";
import { useAppStore } from "../store";
import type { LogEntry } from "../types";

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

export default function LogView() {
  const logs = useAppStore((s) => s.logs);
  const theme = useAppStore((s) => s.theme);
  const clearLogs = useAppStore((s) => s.clearLogs);
  const [filter, setFilter] = useState<FilterKey>("all");
  const [autoScroll, setAutoScroll] = useState(true);
  const boxRef = useRef<HTMLDivElement>(null);

  const visible = useMemo(() => {
    const levels = FILTER_LEVELS[filter];
    if (!levels) return logs;
    return logs.filter((l: LogEntry) => levels.includes(l.level));
  }, [logs, filter]);

  useEffect(() => {
    if (autoScroll && boxRef.current) {
      boxRef.current.scrollTop = boxRef.current.scrollHeight;
    }
  }, [visible, autoScroll]);

  return (
    <div style={{ display: "flex", flexDirection: "column", flex: 1, minHeight: 0 }}>
      <div className="log-toolbar">
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
