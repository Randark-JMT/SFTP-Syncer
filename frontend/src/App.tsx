import { useCallback, useEffect, useState } from "react";
import { Layout, Tabs, Switch, Popover, Button, Space, Tag } from "antd";
import {
  MoonOutlined,
  SunOutlined,
  QuestionCircleOutlined,
  CloudDownloadOutlined,
} from "@ant-design/icons";
// @ts-ignore -- wailsjs 在 wails 构建时生成
import { EventsOn } from "../wailsjs/runtime/runtime";
import { api } from "./bindings";
import { useAppStore } from "./store";
import type { HostConfig, LogEntry, ProgressEvent } from "./types";
import HostSidebar from "./components/HostSidebar";
import HostEditDrawer from "./components/HostEditDrawer";
import TaskTable from "./components/TaskTable";
import LogView from "./components/LogView";

const RULES_TEXT = (
  <div style={{ maxWidth: 380 }}>
    <p style={{ fontWeight: 600 }}>同步规则</p>
    <ul style={{ paddingLeft: 18, margin: 0 }}>
      <li>仅处理远程根目录下各子文件夹中的常规文件；根目录下的脚本、配置等散文件会被跳过。</li>
      <li>仅同步 UTC 修改时间早于当前时间 30 分钟的文件，避免与仍在写入的文件竞争。</li>
      <li>下载后保留目录结构并删除远程源文件。</li>
      <li>跳过以点（.）开头的隐藏目录。</li>
      <li>每台主机使用独立的 SSH 连接池（10 个下载 Worker），多主机并行互不干扰。</li>
    </ul>
  </div>
);

export default function App() {
  const version = useAppStore((s) => s.version);
  const setVersion = useAppStore((s) => s.setVersion);
  const hosts = useAppStore((s) => s.hosts);
  const statuses = useAppStore((s) => s.statuses);
  const tasks = useAppStore((s) => s.tasks);
  const theme = useAppStore((s) => s.theme);
  const setTheme = useAppStore((s) => s.setTheme);
  const setStatuses = useAppStore((s) => s.setStatuses);
  const setStatus = useAppStore((s) => s.setStatus);
  const applyProgress = useAppStore((s) => s.applyProgress);
  const clearHostTasks = useAppStore((s) => s.clearHostTasks);
  const appendLog = useAppStore((s) => s.appendLog);
  const appendLogs = useAppStore((s) => s.appendLogs);

  const [editing, setEditing] = useState<HostConfig | null>(null);
  const [drawerOpen, setDrawerOpen] = useState(false);

  const openAdd = useCallback(() => {
    setEditing(null);
    setDrawerOpen(true);
  }, []);
  const openEdit = useCallback((host: HostConfig) => {
    setEditing(host);
    setDrawerOpen(true);
  }, []);
  const closeDrawer = useCallback(() => setDrawerOpen(false), []);

  useEffect(() => {
    // 初始加载
    api.getVersion().then(setVersion);
    api.listHosts().then((hs) => useAppStore.getState().setHosts(hs));
    api.hostStatuses().then(setStatuses);
    api.getLogHistory().then((entries: LogEntry[]) => appendLogs(entries));

    // 事件订阅
    const offLog = EventsOn("log", (entry: LogEntry) => appendLog(entry));
    const offProgress = EventsOn("progress", (evt: ProgressEvent) => applyProgress(evt));
    const offStatus = EventsOn("host-status", (data: { id: string; status: string }) =>
      setStatus(data.id, data.status),
    );
    const offClear = EventsOn("tasks-clear", (data: { hostId: string }) =>
      clearHostTasks(data.hostId),
    );

    return () => {
      offLog();
      offProgress();
      offStatus();
      offClear();
    };
  }, [
    setVersion,
    setStatuses,
    setStatus,
    applyProgress,
    clearHostTasks,
    appendLog,
    appendLogs,
  ]);

  const runningCount = hosts.filter((h) => h.id && statuses[h.id] && statuses[h.id] !== "已停止").length;

  return (
    <Layout className="app-shell">
      <Layout.Header className="app-header" style={{ background: "transparent" }}>
          <span className="app-title">SFTP Syncer</span>
          <Tag color="blue">{version}</Tag>
          {runningCount > 0 ? (
            <Tag color="processing">状态：运行中（{runningCount}/{hosts.length} 台主机）</Tag>
          ) : (
            <Tag>状态：就绪</Tag>
          )}
          <div style={{ flex: 1 }} />
          <Space>
            <Popover content={RULES_TEXT} title={null} placement="bottomRight">
              <Button type="text" icon={<QuestionCircleOutlined />}>
                同步规则
              </Button>
            </Popover>
            <Switch
              checkedChildren={<MoonOutlined />}
              unCheckedChildren={<SunOutlined />}
              checked={theme === "dark"}
              onChange={(checked) => setTheme(checked ? "dark" : "light")}
            />
          </Space>
        </Layout.Header>

        <Layout className="app-body">
          <div className="app-sider">
            <HostSidebar onAdd={openAdd} onEdit={openEdit} />
          </div>
          <div className="app-content">
            <Tabs
              className="app-tabs"
              defaultActiveKey="tasks"
              items={[
                {
                  key: "tasks",
                  label: (
                    <span>
                      <CloudDownloadOutlined /> 下载任务
                      {tasks.length > 0 && (
                        <Tag style={{ marginLeft: 6 }}>{tasks.length}</Tag>
                      )}
                    </span>
                  ),
                  children: <TaskTable />,
                },
                {
                  key: "logs",
                  label: "运行日志",
                  children: <LogView />,
                },
              ]}
            />
          </div>
        </Layout>

      <HostEditDrawer open={drawerOpen} initial={editing} onClose={closeDrawer} />
    </Layout>
  );
}
