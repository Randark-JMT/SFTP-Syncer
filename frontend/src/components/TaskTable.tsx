import { Empty, Progress, Table, Tag, Typography } from "antd";
import type { ColumnsType } from "antd/es/table";
import { useAppStore } from "../store";
import { formatFileSize, taskStateLabel, type TaskEntry, type TaskState } from "../types";

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

  if (tasks.length === 0) {
    return (
      <div style={{ flex: 1, display: "flex", alignItems: "center", justifyContent: "center" }}>
        <Empty description="当前没有下载任务" />
      </div>
    );
  }

  return (
    <Table
      style={{ flex: 1, minHeight: 0 }}
      size="small"
      rowKey="key"
      columns={columns}
      dataSource={tasks}
      pagination={false}
      sticky
    />
  );
}
