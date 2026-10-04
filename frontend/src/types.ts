// 与 Go 侧 internal/config.Config 对应（JSON 标签一致）。
export interface HostConfig {
  id?: string;
  name?: string;
  host: string;
  port: number;
  username: string;
  authMode: "password" | "private_key";
  password: string;
  privateKeyPath: string;
  privateKeyPassphrase: string;
  remoteDir: string;
  localDir: string;
  pollIntervalSeconds: number;
  skipHostKeyValidation: boolean;
  knownHostsPath: string;
}

export const AUTH_PASSWORD = "password" as const;
export const AUTH_PRIVATE_KEY = "private_key" as const;

// 与 Go 侧 Config.DisplayName 保持一致。
export function hostDisplayName(host: HostConfig): string {
  if (host.name && host.name !== "") return host.name;
  if (host.host === "") return "未命名主机";
  return `${host.host}:${host.port}`;
}

export function emptyHostConfig(): HostConfig {
  return {
    name: "",
    host: "",
    port: 22,
    username: "",
    authMode: AUTH_PASSWORD,
    password: "",
    privateKeyPath: "",
    privateKeyPassphrase: "",
    remoteDir: "",
    localDir: "",
    pollIntervalSeconds: 30,
    skipHostKeyValidation: true,
    knownHostsPath: "",
  };
}

// 日志级别，与 Go 侧约定：0=信息 1=成功 2=警告 3=错误。
export interface LogEntry {
  level: number;
  text: string;
}

export type TaskState = "pending" | "active" | "done" | "failed";

export interface ProgressEvent {
  hostId: string;
  hostLabel: string;
  remotePath: string;
  state: TaskState;
  downloaded: number;
  total: number;
  speedBps: number;
}

export interface TaskEntry {
  key: string;
  hostId: string;
  hostLabel: string;
  remotePath: string;
  state: TaskState;
  downloaded: number;
  total: number;
  speedBps: number;
}

export function taskKey(hostId: string, remotePath: string): string {
  return hostId + "\0" + remotePath;
}

export function formatFileSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  if (bytes < 1024 * 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
  return `${(bytes / (1024 * 1024 * 1024)).toFixed(2)} GB`;
}

export const taskStateLabel: Record<TaskState, string> = {
  pending: "待执行",
  active: "下载中",
  done: "已完成",
  failed: "失败",
};
