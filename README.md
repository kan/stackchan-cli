# stackchan-cli

LAN 内から **M5 ｽﾀｯｸﾁｬﾝ**（M5Stack 公式 AI デスクトップロボット）を操作する CLI。
[stackchan-mcp](https://github.com/kisaragi-mochi/stackchan-mcp) gateway を子プロセスとして起動し、
stdio MCP (JSON-RPC 2.0) 経由で首振り・表情・LED・カメラ等のツールを呼び出します。

> ⚠️ **実験プロジェクト（WIP）です。** 個人の検証用として作られており、API・コマンド・
> 構成は予告なく変わります。動作は特定環境（Windows + 公式 StackChan に stackchan-mcp
> ファームを書込んだ機体）でのみ確認しています。無保証（MIT）。

## アーキテクチャ

```
stackchan-cli (Go)  --stdio MCP-->  stackchan-mcp gateway (Python)  --WebSocket-->  StackChan (ESP32-S3)
```

- CLI は gateway を**子プロセス起動**し、`initialize` → `tools/call` を stdin/stdout で送受信。
- gateway は WebSocket サーバ(:8765) を持ち、デバイスがそこへ接続してくる。
- 依存ゼロ（Go 標準ライブラリのみ）。

## 前提

- **Go** 1.22+（開発は 1.26）
- **stackchan-mcp gateway**（[GitHub main 版](https://github.com/kisaragi-mochi/stackchan-mcp) 推奨。mDNS 広告対応）
  ```
  uv tool install --force "git+https://github.com/kisaragi-mochi/stackchan-mcp#subdirectory=gateway"
  ```
- **StackChan** に stackchan-mcp ファームを書込み、WebSocket Gateway URL を本機へ向けてあること
  （captive portal の **Advanced** タブで `ws://<PC-LAN-IP>:8765/`。詳細は `CLAUDE.md`）。

## ビルド

```
go build -o stackchan-cli .
```

## 使い方

```bash
# 環境変数（トークンは空＝認証なしでも可。写真用に自機 LAN IP を指定）
export STACKCHAN_TOKEN=        # 省略可
export VISION_HOST=<PC-LAN-IP> # take_photo 用

./stackchan-cli status                       # gateway/デバイス接続状態（デバイス不要）
./stackchan-cli tools                        # 公開ツール一覧（デバイス不要）
./stackchan-cli wait                         # デバイス接続を待つ（疎通確認）
./stackchan-cli avatar happy                 # 表情変更 idle|happy|thinking|sad|surprised|embarrassed|off
./stackchan-cli move-head --yaw 30 --pitch 40
./stackchan-cli all-leds --r 0 --g 0 --b 255
./stackchan-cli led --index 0 --r 255 --g 0 --b 0
./stackchan-cli call <tool> --json '{"...":"..."}'   # 任意ツールを生 JSON で
```

デバイス系コマンドは、gateway 起動後にデバイスが再接続するまで待ってから実行します。

## 既知の制限

- **one-shot 方式**：コマンド毎に gateway を起動/終了するため、デバイス再接続待ちで遅い。
  常駐（REPL / daemon）化は今後の課題。
- `say` / `listen`（TTS/STT）は gateway 側に optional extra を入れるまで使えません。
- mDNS 自動検出が通らない環境では、デバイスに WebSocket URL を明示設定してください。

## ドキュメント

- `CLAUDE.md` … 調査・意思決定ログと、書込み〜接続までの詳細な手順（日本語）。
- `docs/gateway-tools.json` … gateway が公開するツールのスキーマ。

## ライセンス / 謝辞

- MIT License（`LICENSE`）。
- 上流: [kisaragi-mochi/stackchan-mcp](https://github.com/kisaragi-mochi/stackchan-mcp),
  [78/xiaozhi-esp32](https://github.com/78/xiaozhi-esp32),
  [stack-chan](https://github.com/stack-chan/stack-chan)。
