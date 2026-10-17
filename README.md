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

- **Go** 1.27+
- **stackchan-mcp gateway 0.18.0**（firmware 1.17.0 と組で使う）。発話（`[tts]`）と聞き取り
  （`[stt-faster-whisper]`）の追加モジュール込みで入れる。`av<19` は必須（PyAV 19 は
  faster-whisper 1.2.1 が渡す `metadata_errors` 引数を受け付けず、文字起こしが失敗する）:
  ```
  uv tool install --force --python 3.12 "stackchan-mcp[tts,stt-faster-whisper]==0.18.0" --with "av<19"
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

### daemon（常駐・タッチ反応・即時コマンド）

`daemon` は gateway を**常駐**させ、(1) 頭部タッチに**自動反応**し、(2) ローカル IPC
（`127.0.0.1:8770`）で `stackchan-cli <cmd>` を**即時転送**する（gateway 再起動なし＝~0.04s、
ポート競合も解消）。

```
stackchan-cli daemon            # 常駐起動（Ctrl-C で停止）
# 別ターミナルから（daemon があれば自動で転送される）:
stackchan-cli avatar happy
stackchan-cli say "やっほー"
```

- **タッチ反応**：本体（firmware 1.9 以降）が頭部タッチを自分で検出し、gateway が
  `stackchan/event` 通知として daemon に届ける（daemon が `~/.stackchan/notify.yml` を作り、
  `STACKCHAN_NOTIFY_CONFIG` で gateway に渡す）。daemon は接触時間で tap/stroke を分類して反応
  （〜1秒=tap / それ以上=stroke、`--tap-max <ms>` で変更、`--touch=false` で無効）。
  本体への問い合わせ（ポーリング）はしないので、通信が遅くても取りこぼさない。
- **自作アバターの自動送信**：本体が接続するたび（再起動後も）、`~/.stackchan/avatar/*.png`（14 枚）を
  RGB565 に変換して `load_avatar_set` で送る。送った顔は本体の RAM にしか残らないため。
  `--avatar=false` で無効。手動では `stackchan-cli avatar-load`。
- **反応は編集可能なスクリプト**：daemon 初回起動時に `~/.stackchan/reactions/tap.txt` と `stroke.txt`
  を雛形生成。中身は REPL コマンド（`avatar`/`mouth`/`led`/`say`/`sleep` など）を行で並べるだけ。
  **再ビルド不要**で、ジェスチャ発火時にその場のファイル内容が実行される（`--reactions <dir>` で場所変更）。
- daemon 稼働中は **one-shot も `repl` も `source` も**自動で IPC 転送される（その常駐 gateway を共有。
  `repl` は daemon があれば各行を転送、無ければ自前 gateway を起動）。転送したコマンドが失敗すると
  終了コードは 1 になる。
- daemon は起動時に **VOICEVOX エンジンをウィンドウ無しで自動起動**する（後述）。
- daemon のログ（stderr）は各行に時刻が付く。
- gateway・VOICEVOX エンジンなどの子プロセスは Windows の Job オブジェクトに入れてあり、
  daemon が強制終了されても一緒に終了する（ポート 8765/8766/50021 を掴んだまま残らない）。

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

- **one-shot 方式**：daemon が無いと、コマンド毎に gateway を起動/終了するため、デバイス再接続待ち
  （最大90s）で遅い。連続操作には **`daemon`** か **`repl`（gateway 常駐）** を使う。
- gateway がエラーを `{"error": ...}` の形で返したとき（デバイス未接続、TTS 失敗など）は失敗として
  扱い、終了コード 1 を返す。
- `say`（TTS）は VOICEVOX エンジン＋gateway に opuslib（+Windows は opus.dll）が要る。エンジンが
  動いていなければ CLI が `vv-engine\run.exe` をウィンドウ無しで自動起動する。
  セットアップは [`docs/tts-voicevox.md`](docs/tts-voicevox.md)。既定話者は 8=春日部つむぎ、
  発話後は自動で表情 `embarrassed`（`say --speaker N` / `say --face <name>` で変更可）。
- `listen`（聞き取り）: 本体のマイクで録音し、gateway が PC 内で文字起こしする（faster-whisper）。
  `stackchan-cli listen [--sec 5] [--lang ja] [--model small] [--motion none]`。
  既定モデルは `small`（CLI が `STACKCHAN_FASTER_WHISPER_MODEL=small` を gateway に渡す。`base` では
  「スタックチャン」が「スタッフちゃん」になった）。初回はモデル（約 460MB）をダウンロードする。
  CPU で録音 6 秒＋文字起こし約 4 秒。聞き取り中の表情切り替え（`--motion face-only`）は、本体の応答が
  遅いと失敗することがあるので既定でオフ。
- mDNS 自動検出が通らない環境では、デバイスに WebSocket URL を明示設定してください。
- **PC の IP が変わったとき**（デバイスが gateway に繋がらなくなる）：
  - デバイスが繋がっている間に変えるなら
    `stackchan-cli call gateway_config_set --json '{"url":"ws://<PC-IP>:8765/"}'`（次の再接続から有効）。
  - 繋がらなくなった後は、USB 接続して
    `py scripts/nvs_set_ws_url.py --port COM3 --url ws://<PC-IP>:8765/`。NVS の `websocket.url` だけを
    書き換えるので Wi-Fi 設定は残る（captive portal に入る必要がない）。元の URL より長い URL は不可。
- 想定バージョン：firmware **1.17.0** / gateway **0.18.0**（タッチ通知・`load_avatar_set`・
  `gateway_config_set` はこの組み合わせが前提）。

## ドキュメント

- `CLAUDE.md` … 調査・意思決定ログと、書込み〜接続までの詳細な手順（日本語）。
- `docs/gateway-tools.json` … gateway が公開するツールのスキーマ。

## ライセンス / 謝辞

- MIT License（`LICENSE`）。
- 上流: [kisaragi-mochi/stackchan-mcp](https://github.com/kisaragi-mochi/stackchan-mcp),
  [78/xiaozhi-esp32](https://github.com/78/xiaozhi-esp32),
  [stack-chan](https://github.com/stack-chan/stack-chan)。
