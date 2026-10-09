import { create } from "zustand";
import type { HostConfig, LogEntry, ProgressEvent, TaskEntry } from "./types";
import { taskKey } from "./types";

const MAX_LOGS = 500;

type ThemeMode = "light" | "dark";

interface AppState {
  version: string;
  hosts: HostConfig[];
  statuses: Record<string, string>;
  tasks: TaskEntry[];
  logs: LogEntry[];
  theme: ThemeMode;

  setVersion: (v: string) => void;
  setHosts: (hosts: HostConfig[]) => void;
  upsertHost: (host: HostConfig) => void;
  removeHost: (id: string) => void;
  setStatuses: (m: Record<string, string>) => void;
  setStatus: (id: string, status: string) => void;
  applyProgress: (evt: ProgressEvent) => void;
  clearHostTasks: (hostId: string) => void;
  appendLog: (entry: LogEntry) => void;
  appendLogs: (entries: LogEntry[]) => void;
  clearLogs: () => void;
  setTheme: (t: ThemeMode) => void;
}

function applyEvent(tasks: TaskEntry[], evt: ProgressEvent): TaskEntry[] {
  const key = taskKey(evt.hostId, evt.remotePath);
  const idx = tasks.findIndex((t) => t.key === key);

  if (evt.state === "done") {
    // 完成的任务从列表移除（与旧版行为一致）。
    if (idx < 0) return tasks;
    return tasks.filter((t) => t.key !== key);
  }

  if (idx >= 0) {
    const cur = tasks[idx];
    const next: TaskEntry = {
      ...cur,
      state: evt.state,
      // Progress events are snapshots. A retry starts from byte zero and a
      // periodic sample may legitimately report zero bytes.
      downloaded: evt.downloaded,
      total: evt.total > 0 ? evt.total : cur.total,
      // Keep the backend's zero speed so stale values disappear when the
      // transfer pauses or the watchdog detects a stall.
      speedBps: evt.speedBps,
    };
    const copy = tasks.slice();
    copy[idx] = next;
    return copy;
  }

  return [
    ...tasks,
    {
      key,
      hostId: evt.hostId,
      hostLabel: evt.hostLabel,
      remotePath: evt.remotePath,
      state: evt.state,
      downloaded: evt.downloaded,
      total: evt.total,
      speedBps: evt.speedBps,
    },
  ];
}

export const useAppStore = create<AppState>((set) => ({
  version: "dev",
  hosts: [],
  statuses: {},
  tasks: [],
  logs: [],
  theme: (localStorage.getItem("theme") as ThemeMode) || "light",

  setVersion: (v) => set({ version: v }),
  setHosts: (hosts) => set({ hosts: [...hosts] }),
  upsertHost: (host) =>
    set((s) => {
      if (!host.id) return s;
      const idx = s.hosts.findIndex((h) => h.id === host.id);
      if (idx < 0) return { hosts: [...s.hosts, host] };
      const copy = s.hosts.slice();
      copy[idx] = host;
      return { hosts: copy };
    }),
  removeHost: (id) =>
    set((s) => ({
      hosts: s.hosts.filter((h) => h.id !== id),
      statuses: Object.fromEntries(Object.entries(s.statuses).filter(([k]) => k !== id)),
      tasks: s.tasks.filter((t) => t.hostId !== id),
    })),
  setStatuses: (m) => set({ statuses: { ...m } }),
  setStatus: (id, status) =>
    set((s) => ({ statuses: { ...s.statuses, [id]: status } })),
  applyProgress: (evt) => set((s) => ({ tasks: applyEvent(s.tasks, evt) })),
  clearHostTasks: (hostId) =>
    set((s) => ({ tasks: s.tasks.filter((t) => t.hostId !== hostId) })),
  appendLog: (entry) =>
    set((s) => ({
      logs: [...s.logs, entry].slice(-MAX_LOGS),
    })),
  appendLogs: (entries) =>
    set((s) => ({
      logs: entries.length > 0 ? [...s.logs, ...entries].slice(-MAX_LOGS) : s.logs,
    })),
  clearLogs: () => set({ logs: [] }),
  setTheme: (t) => {
    localStorage.setItem("theme", t);
    set({ theme: t });
  },
}));
