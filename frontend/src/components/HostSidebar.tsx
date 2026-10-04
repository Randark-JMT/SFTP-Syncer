import { useState } from "react";
import {
  Badge,
  Button,
  Card,
  Dropdown,
  Empty,
  Input,
  Modal,
  Space,
  Typography,
  message,
} from "antd";
import {
  CaretRightOutlined,
  PauseOutlined,
  PlusOutlined,
  EditOutlined,
  DeleteOutlined,
  MoreOutlined,
  PlayCircleOutlined,
  StopOutlined,
} from "@ant-design/icons";
import { api } from "../bindings";
import { useAppStore } from "../store";
import type { HostConfig } from "../types";

const STATUS_BADGE: Record<string, { status: "default" | "processing" | "error" | "warning"; text: string }> = {
  已停止: { status: "default", text: "已停止" },
  运行中: { status: "processing", text: "运行中" },
  "运行中（出错）": { status: "error", text: "运行中（出错）" },
  正在停止: { status: "warning", text: "正在停止" },
};

function hostLabel(h: HostConfig): string {
  if (h.name && h.name.trim()) return h.name.trim();
  return `${h.host}:${h.port}`;
}

export default function HostSidebar({
  onAdd,
  onEdit,
}: {
  onAdd: () => void;
  onEdit: (host: HostConfig) => void;
}) {
  const hosts = useAppStore((s) => s.hosts);
  const statuses = useAppStore((s) => s.statuses);
  const removeHost = useAppStore((s) => s.removeHost);
  const [search, setSearch] = useState("");
  const [messageApi, contextHolder] = message.useMessage();

  const anyRunning = hosts.some((h) => h.id && statuses[h.id] && statuses[h.id] !== "已停止");

  const isRunning = (id?: string) => !!id && !!statuses[id] && statuses[id] !== "已停止";

  const handleStart = async (id?: string) => {
    if (!id) return;
    try {
      await api.startHost(id);
    } catch (err) {
      messageApi.error(String(err));
    }
  };

  const handleStop = async (id?: string) => {
    if (!id) return;
    try {
      await api.stopHost(id);
    } catch (err) {
      messageApi.error(String(err));
    }
  };

  const handleDelete = (host: HostConfig) => {
    if (!host.id) return;
    if (isRunning(host.id)) {
      messageApi.warning("主机正在运行，请先停止同步任务后再删除");
      return;
    }
    Modal.confirm({
      title: "确认删除",
      content: `确定删除主机「${hostLabel(host)}」吗？`,
      okText: "删除",
      okButtonProps: { danger: true },
      cancelText: "取消",
      onOk: async () => {
        try {
          await api.removeHost(host.id!);
          removeHost(host.id!);
        } catch (err) {
          messageApi.error(String(err));
        }
      },
    });
  };

  const filtered = hosts.filter((h) => {
    if (!search.trim()) return true;
    const q = search.trim().toLowerCase();
    return (
      (h.name ?? "").toLowerCase().includes(q) ||
      h.host.toLowerCase().includes(q) ||
      h.username.toLowerCase().includes(q)
    );
  });

  return (
    <>
      {contextHolder}
      <Space.Compact style={{ width: "100%" }}>
        <Input
          allowClear
          placeholder="搜索主机…"
          prefix={<></>}
          value={search}
          onChange={(e) => setSearch(e.target.value)}
        />
      </Space.Compact>

      <Button type="dashed" block icon={<PlusOutlined />} onClick={onAdd}>
        添加主机
      </Button>

      <Space style={{ width: "100%" }}>
        <Button
          icon={<PlayCircleOutlined />}
          disabled={hosts.length === 0}
          onClick={() => api.startAll().catch((e) => messageApi.error(String(e)))}
        >
          全部开始
        </Button>
        <Button
          icon={<StopOutlined />}
          danger
          disabled={!anyRunning}
          onClick={() => api.stopAll().catch((e) => messageApi.error(String(e)))}
        >
          全部停止
        </Button>
      </Space>

      {filtered.length === 0 ? (
        <Empty
          style={{ marginTop: 48 }}
          description={hosts.length === 0 ? "尚未添加主机" : "无匹配结果"}
        />
      ) : (
        filtered.map((host) => {
          const id = host.id ?? "";
          const status = statuses[id] ?? "已停止";
          const badge = STATUS_BADGE[status] ?? STATUS_BADGE["已停止"];
          const running = isRunning(id);
          return (
            <Card
              key={id}
              className="host-card"
              size="small"
              title={
                <span style={{ fontSize: 14 }}>
                  {hostLabel(host)}
                  <Typography.Text
                    type="secondary"
                    style={{ marginLeft: 8, fontSize: 12, fontWeight: 400 }}
                  >
                    {host.username}
                  </Typography.Text>
                </span>
              }
              extra={<Badge status={badge.status} text={badge.text} />}
              actions={[
                <Button
                  key="start"
                  type="primary"
                  size="small"
                  icon={<CaretRightOutlined />}
                  disabled={running}
                  onClick={() => handleStart(id)}
                >
                  开始
                </Button>,
                <Button
                  key="stop"
                  size="small"
                  icon={<PauseOutlined />}
                  disabled={!running}
                  onClick={() => handleStop(id)}
                >
                  停止
                </Button>,
                <Dropdown
                  key="more"
                  menu={{
                    items: [
                      {
                        key: "edit",
                        icon: <EditOutlined />,
                        label: "编辑…",
                        disabled: running,
                        onClick: () => onEdit(host),
                      },
                      {
                        key: "delete",
                        icon: <DeleteOutlined />,
                        label: "删除",
                        danger: true,
                        disabled: running,
                        onClick: () => handleDelete(host),
                      },
                    ],
                  }}
                  trigger={["click"]}
                >
                  <Button size="small" type="text" icon={<MoreOutlined />} />
                </Dropdown>,
              ]}
            >
              <Typography.Paragraph style={{ marginBottom: 4 }} ellipsis={{ tooltip: host.remoteDir }}>
                <Typography.Text type="secondary">远程：</Typography.Text>
                <Typography.Text code>{host.remoteDir}</Typography.Text>
              </Typography.Paragraph>
              <Typography.Paragraph style={{ marginBottom: 0 }} ellipsis={{ tooltip: host.localDir }}>
                <Typography.Text type="secondary">本地：</Typography.Text>
                <Typography.Text code>{host.localDir}</Typography.Text>
              </Typography.Paragraph>
              <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                轮询间隔：{host.pollIntervalSeconds} 秒
              </Typography.Text>
            </Card>
          );
        })
      )}
    </>
  );
}
