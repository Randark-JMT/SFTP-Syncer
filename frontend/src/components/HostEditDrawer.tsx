import { useEffect, useState } from "react";
import {
  Button,
  Drawer,
  Form,
  Input,
  InputNumber,
  Radio,
  Space,
  Switch,
  Typography,
  message,
} from "antd";
import { FolderOpenOutlined, FileOutlined } from "@ant-design/icons";
import { api } from "../bindings";
import { useAppStore } from "../store";
import { emptyHostConfig, AUTH_PASSWORD, AUTH_PRIVATE_KEY, type HostConfig } from "../types";

interface FormValues {
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

function toFormValues(cfg: HostConfig): FormValues {
  return {
    name: cfg.name ?? "",
    host: cfg.host,
    port: cfg.port,
    username: cfg.username,
    authMode: cfg.authMode,
    password: cfg.password,
    privateKeyPath: cfg.privateKeyPath,
    privateKeyPassphrase: cfg.privateKeyPassphrase,
    remoteDir: cfg.remoteDir,
    localDir: cfg.localDir,
    pollIntervalSeconds: cfg.pollIntervalSeconds,
    skipHostKeyValidation: cfg.skipHostKeyValidation,
    knownHostsPath: cfg.knownHostsPath,
  };
}

export default function HostEditDrawer({
  open,
  initial,
  onClose,
}: {
  open: boolean;
  initial: HostConfig | null;
  onClose: () => void;
}) {
  const [form] = Form.useForm<FormValues>();
  const [saving, setSaving] = useState(false);
  const upsertHost = useAppStore((s) => s.upsertHost);
  const [messageApi, contextHolder] = message.useMessage();

  useEffect(() => {
    if (open) {
      form.resetFields();
      form.setFieldsValue(toFormValues(initial ?? emptyHostConfig()));
    }
  }, [open, initial, form]);

  const authMode = Form.useWatch("authMode", form) ?? AUTH_PASSWORD;
  const skipCheck = Form.useWatch("skipHostKeyValidation", form) ?? true;

  const handleOk = async () => {
    try {
      const values = await form.validateFields();
      setSaving(true);
      const cfg: HostConfig = { ...(initial ?? {}), ...values };
      try {
        if (initial?.id) {
          await api.updateHost({ ...cfg, id: initial.id });
          // 更新后回读，保证侧栏显示最新配置。
          const hosts = await api.listHosts();
          useAppStore.getState().setHosts(hosts);
        } else {
          const added = await api.addHost(cfg);
          upsertHost(added);
          const statuses = await api.hostStatuses();
          useAppStore.getState().setStatuses(statuses);
        }
        onClose();
      } catch (err) {
        messageApi.error({
          content: String(err),
          style: { whiteSpace: "pre-wrap" },
        });
      } finally {
        setSaving(false);
      }
    } catch {
      // 表单校验失败，antd 已标注错误字段
    }
  };

  return (
    <Drawer
      title={initial?.id ? "编辑主机" : "添加主机"}
      width={480}
      open={open}
      onClose={onClose}
      destroyOnClose
      footer={
        <Space style={{ display: "flex", justifyContent: "flex-end" }}>
          <Button onClick={onClose}>取消</Button>
          <Button type="primary" loading={saving} onClick={handleOk}>
            确定
          </Button>
        </Space>
      }
    >
      {contextHolder}
      <Form form={form} layout="vertical" initialValues={toFormValues(emptyHostConfig())}>
        <Form.Item name="name" label="名称（可选，用于区分主机）">
          <Input placeholder="例如：公司备份服务器" />
        </Form.Item>
        <Space style={{ display: "flex" }} align="start">
          <Form.Item
            name="host"
            label="服务器地址"
            rules={[{ required: true, message: "请填写服务器地址" }]}
            style={{ flex: 1, minWidth: 200 }}
          >
            <Input placeholder="sftp.example.com" />
          </Form.Item>
          <Form.Item name="port" label="端口" rules={[{ required: true, message: " " }]}>
            <InputNumber min={1} max={65535} style={{ width: 96 }} />
          </Form.Item>
        </Space>
        <Form.Item
          name="username"
          label="用户名"
          rules={[{ required: true, message: "请填写用户名" }]}
        >
          <Input />
        </Form.Item>

        <Form.Item name="authMode" label="认证方式">
          <Radio.Group
            options={[
              { value: AUTH_PASSWORD, label: "密码认证" },
              { value: AUTH_PRIVATE_KEY, label: "私钥认证" },
            ]}
          />
        </Form.Item>
        {authMode === AUTH_PASSWORD ? (
          <Form.Item
            name="password"
            label="密码"
            rules={[{ required: true, message: "请填写密码" }]}
          >
            <Input.Password />
          </Form.Item>
        ) : (
          <>
            <Form.Item
              name="privateKeyPath"
              label="私钥文件路径"
              rules={[{ required: true, message: "请填写或选择私钥文件" }]}
            >
              <Input
                addonAfter={
                  <FileOutlined
                    style={{ cursor: "pointer" }}
                    onClick={async () => {
                      const p = await api.selectFile("选择私钥文件");
                      if (p) form.setFieldValue("privateKeyPath", p);
                    }}
                  />
                }
              />
            </Form.Item>
            <Form.Item name="privateKeyPassphrase" label="私钥口令（可选，私钥未加密时留空）">
              <Input.Password />
            </Form.Item>
          </>
        )}

        <Form.Item
          name="remoteDir"
          label="远程目录"
          rules={[{ required: true, message: "请填写远程目录" }]}
        >
          <Input placeholder="/data/upload" />
        </Form.Item>
        <Form.Item
          name="localDir"
          label="本地目录"
          rules={[{ required: true, message: "请填写本地目录" }]}
        >
          <Input
            placeholder="D:\downloads"
            addonAfter={
              <FolderOpenOutlined
                style={{ cursor: "pointer" }}
                onClick={async () => {
                  const p = await api.selectDirectory("选择本地目录");
                  if (p) form.setFieldValue("localDir", p);
                }}
              />
            }
          />
        </Form.Item>
        <Form.Item
          name="pollIntervalSeconds"
          label="轮询间隔（秒）"
          rules={[{ required: true, message: " " }]}
          extra="相邻两轮扫描的间隔，范围 5–3600 秒"
        >
          <InputNumber min={5} max={3600} style={{ width: 160 }} />
        </Form.Item>

        <Form.Item
          name="skipHostKeyValidation"
          label="主机密钥校验"
          valuePropName="checked"
          extra="跳过校验更省事，但存在中间人风险；关闭校验时需提供 known_hosts 路径"
        >
          <Switch checkedChildren="跳过" unCheckedChildren="校验" />
        </Form.Item>
        {!skipCheck && (
          <Form.Item
            name="knownHostsPath"
            label="known_hosts 路径（可选，默认 ~/.ssh/known_hosts）"
            rules={[{ required: false }]}
          >
            <Input
              addonAfter={
                <FileOutlined
                  style={{ cursor: "pointer" }}
                  onClick={async () => {
                    const p = await api.selectFile("选择 known_hosts 文件");
                    if (p) form.setFieldValue("knownHostsPath", p);
                  }}
                />
              }
            />
          </Form.Item>
        )}

        <Typography.Paragraph type="secondary" style={{ fontSize: 12 }}>
          密码与私钥口令会以明文保存在本机配置文件中，请确保机器安全。
        </Typography.Paragraph>
      </Form>
    </Drawer>
  );
}
