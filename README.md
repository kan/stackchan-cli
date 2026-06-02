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
- 依存は最小限（CLI 本体は標準ライブラリのみ。REPL の補完/履歴に `chzyer/readline` を使用）。

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
export VISION_HOST=<PC-LAN-IP> # take_photo 用（未設定なら CLI が LAN IP を自動検出）

./stackchan-cli status                       # gateway/デバイス接続状態（デバイス不要）
./stackchan-cli tools                        # 公開ツール一覧（デバイス不要）
./stackchan-cli wait                         # デバイス接続を待つ（疎通確認）
./stackchan-cli avatar happy                 # 表情変更 idle|happy|thinking|sad|surprised|embarrassed|off
./stackchan-cli mouth open                   # 口形 closed|half|open|e|u
./stackchan-cli blink off                    # まばたき on|off
./stackchan-cli move-head --yaw 30 --pitch 40
./stackchan-cli all-leds --r 0 --g 0 --b 255
./stackchan-cli led --index 0 --r 255 --g 0 --b 0
./stackchan-cli clear-leds                   # 全消灯
./stackchan-cli volume 60                    # スピーカ音量 0-100
./stackchan-cli brightness 80                # 画面輝度 0-100
./stackchan-cli photo --question "何が見える？" --open  # 撮影→ ~/.stackchan/captures に保存し既定ビューアで表示（VISION_HOST は未設定なら自動検出）
./stackchan-cli call <tool> --json '{"...":"..."}'   # 任意ツールを生 JSON で
```

デバイス系コマンドは、gateway 起動後にデバイスが再接続するまで待ってから実行します。

### REPL（連続操作向け・推奨）

`repl` は gateway を**1回だけ起動して常駐**させ、デバイスを繋ぎっぱなしにするので、
コマンド毎の再接続待ちが無く即時実行できます。

```
./stackchan-cli repl
# gateway up; waiting for device to connect...
# device connected ✓
# stackchan> avatar happy
# stackchan> move-head --yaw 30 --pitch 40
# stackchan> all-leds --r 0 --g 255 --b 0
# stackchan> say "こんにちは"
# stackchan> quit
```

`"..."` / `'...'` のクォートが使えるので `say "hello world"` や
`call <tool> --json '{"k":"v"}'` もそのまま入力できます。

REPL の編集機能（`chzyer/readline`）:
- **Tab 補完**：コマンド名・`avatar` の face 名・主要フラグ・`call` のツール名
- **履歴**：↑↓ で呼び出し、`~/.stackchan/repl_history` に永続化
- **Ctrl-R**：履歴の逆引き検索（大文字小文字無視）
- **行編集**：←→ / Home/End / Ctrl-A・E・U・W 等、**Ctrl-C** で行クリア（空行で終了）、**Ctrl-D** で終了

### スクリプト / 演出

REPL とスクリプトでは次のメタコマンドが使えます：
- `#` 始まりの行はコメント
- `sleep <ms>` … 待機（ミリ秒）
- `repeat <n> <command...>` … コマンドを n 回
- `source <file>` … ファイルの各行を実行

表情×口×首×LED を時間差で並べて“振り付け”が作れます。one-shot の
`stackchan-cli source <file>` なら **gateway を1回だけ起動して全行を実行**します。

例（`examples/greet.txt`）:
```
stackchan-cli source examples/greet.txt
# または REPL 内で:  source examples/greet.txt
```

## 既知の制限

- **one-shot 方式**：コマンド毎に gateway を起動/終了するため、デバイス再接続待ち（最大90s）で遅い。
  連続操作には **`repl`（gateway 常駐）** を使うと即時実行。daemon+IPC 化（バックグラウンド常駐）は今後の課題。
- `say`（TTS）は VOICEVOX エンジン稼働＋gateway に opuslib（+Windows は opus.dll）が要る。
  セットアップは [`docs/tts-voicevox.md`](docs/tts-voicevox.md)。既定話者は 8=春日部つむぎ、
  発話後は自動で表情 `embarrassed`（`say --speaker N` / `say --face <name>` で変更可）。
  `listen`（STT）は別途 STT extra が必要（未セットアップ）。
- mDNS 自動検出が通らない環境では、デバイスに WebSocket URL を明示設定してください。

## ドキュメント

- `CLAUDE.md` … 調査・意思決定ログと、書込み〜接続までの詳細な手順（日本語）。
- `docs/gateway-tools.json` … gateway が公開するツールのスキーマ。

## ライセンス / 謝辞

- MIT License（`LICENSE`）。
- 上流: [kisaragi-mochi/stackchan-mcp](https://github.com/kisaragi-mochi/stackchan-mcp),
  [78/xiaozhi-esp32](https://github.com/78/xiaozhi-esp32),
  [stack-chan](https://github.com/stack-chan/stack-chan)。
