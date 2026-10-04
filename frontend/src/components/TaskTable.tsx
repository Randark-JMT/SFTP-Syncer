import { useMemo, useState } from "react";
import { Empty, Progress, Select, Table, Tag, Typography } from "antd";
import { CloudDownloadOutlined } from "@ant-design/icons";
import type { ColumnsType } from "antd/es/table";
import { useAppStore } from "../store";
import {
  formatFileSize,
  hostDisplayName,
  taskStateLabel,
  type TaskEntry,
  type TaskState,
} from "../types";

const STATE_TAG: Record<TaskState, { color: string; text: string }> = {
  pending: { color: "default", text: taskStateLabel.pending },
  active: { color: "processing", text: taskStateLabel.active },
  done: { color: "success", text: taskStateLabel.done },
  failed: { color: "error", text: taskStateLabel.failed },
};

const columns: ColumnsType<TaskEntry> = [
  {
    title: "主机",
    dataIndex: "hostLabel",
    width: 140,
    ellipsis: true,
  },
  {
    title: "状态",
    dataIndex: "state",
    width: 90,
    render: (state: TaskState) => <Tag color={STATE_TAG[state].color}>{STATE_TAG[state].text}</Tag>,
  },
  {
    title: "路径",
    dataIndex: "remotePath",
    ellipsis: true,
    render: (p: string) => <Typography.Text code>{p}</Typography.Text>,
  },
  {
    title: "大小",
    dataIndex: "total",
    width: 100,
    render: (total: number) => (total > 0 ? formatFileSize(total) : "—"),
  },
  {
    title: "进度",
    key: "progress",
    width: 240,
    render: (_: unknown, row: TaskEntry) => {
      if (row.total <= 0) {
        return row.state === "failed" ? "—" : <Progress percent={0} size="small" status="active" />;
      }
      const pct = Math.floor((row.downloaded / row.total) * 100);
      return (
        <Progress
          percent={pct}
          size="small"
          status={row.state === "failed" ? "exception" : row.state === "pending" ? "normal" : "active"}
          format={() => `${formatFileSize(row.downloaded)} / ${formatFileSize(row.total)}`}
        />
      );
    },
  },
  {
    title: "速度",
    dataIndex: "speedBps",
    width: 110,
    render: (speed: number) => (speed >= 1 ? `${formatFileSize(speed)}/s` : "—"),
  },
];

export default function TaskTable() {
  const tasks = useAppStore((s) => s.tasks);
  const hosts = useAppStore((s) => s.hosts);
  const [hostFilter, setHostFilter] = useState<string>("all");

  const hostOptions = useMemo(
    () => [
      { value: "all", label: "全部主机" },
      ...hosts
        .filter((h) => h.id)
        .map((h) => ({ value: h.id as string, label: hostDisplayName(h) })),
    ],
    [hosts],
  );

  // 主机被删除后回退到"全部"。
  const effectiveFilter =
    hostFilter !== "all" && !hosts.some((h) => h.id === hostFilter) ? "all" : hostFilter;

  const visible = useMemo(
    () =>
      effectiveFilter === "all"
        ? tasks
        : tasks.filter((t) => t.hostId === effectiveFilter),
    [tasks, effectiveFilter],
  );

  return (
    <div style={{ display: "flex", flexDirection: "column", flex: 1, minHeight: 0 }}>
      <div className="table-toolbar">
        <span className="section-title">
          <CloudDownloadOutlined /> 下载任务
        </span>
        {tasks.length > 0 && (
          <Tag>
            {visible.length}/{tasks.length}
          </Tag>
        )}
        <div style={{ flex: 1 }} />
        <Select
          size="small"
          value={effectiveFilter}
          onChange={setHostFilter}
          options={hostOptions}
          style={{ minWidth: 160 }}
        />
      </div>
      {visible.length === 0 ? (
        <div
          style={{
            flex: 1,
            display: "flex",
            alignItems: "center",
            justifyContent: "center",
          }}
        >
          <Empty description="当前没有下载任务" />
        </div>
      ) : (
        <div style={{ flex: 1, minHeight: 0, overflow: "auto" }}>
          <Table
            size="small"
            rowKey="key"
            columns={columns}
            dataSource={visible}
            pagination={false}
          />
        </div>
      )}
    </div>
  );
}
