// wailsjs 绑定目录由 `wails build` / `wails dev` 生成到 frontend/wailsjs/。
// 这里做一层带类型的封装，业务代码不直接触碰生成文件。
// @ts-ignore -- 生成目录在首次 wails 构建前不存在
import * as bridge from "../wailsjs/go/main/App";
import type { HostConfig, LogEntry } from "./types";

export const api = {
  listHosts: (): Promise<HostConfig[]> => bridge.ListHosts() as Promise<HostConfig[]>,
  addHost: (cfg: HostConfig): Promise<HostConfig> =>
    bridge.AddHost(cfg as never) as Promise<HostConfig>,
  updateHost: (cfg: HostConfig): Promise<void> =>
    bridge.UpdateHost(cfg as never) as Promise<void>,
  removeHost: (id: string): Promise<void> => bridge.RemoveHost(id) as Promise<void>,
  startHost: (id: string): Promise<void> => bridge.StartHost(id) as Promise<void>,
  stopHost: (id: string): Promise<void> => bridge.StopHost(id) as Promise<void>,
  startAll: (): Promise<void> => bridge.StartAll() as Promise<void>,
  stopAll: (): Promise<void> => bridge.StopAll() as Promise<void>,
  hostStatuses: (): Promise<Record<string, string>> =>
    bridge.HostStatuses() as Promise<Record<string, string>>,
  getLogHistory: (): Promise<LogEntry[]> => bridge.GetLogHistory() as Promise<LogEntry[]>,
  clearLog: (): Promise<void> => bridge.ClearLog() as Promise<void>,
  selectDirectory: (title: string): Promise<string> =>
    bridge.SelectDirectory(title) as Promise<string>,
  selectFile: (title: string): Promise<string> => bridge.SelectFile(title) as Promise<string>,
  getVersion: (): Promise<string> => bridge.GetVersion() as Promise<string>,
};
