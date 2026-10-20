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
  NVIDIA の GPU で文字起こしを速くするなら、CUDA のライブラリも同じ環境に入れる（約 1GB）。
  入っていれば CLI が自動で GPU を使う（`STACKCHAN_FASTER_WHISPER_DEVICE=cpu` で CPU に戻せる）。
  ```
  uv tool install --force --python 3.12 "stackchan-mcp[tts,stt-faster-whisper]==0.18.0" --with "av<19" --with nvidia-cublas-cu12 --with "nvidia-cudnn-cu12==9.*"
  ```
- **StackChan** に stackchan-mcp ファームを書込み、WebSocket Gateway URL を本機へ向けてあること
  （captive portal の **Advanced** タブで `ws://<PC-LAN-IP>:8765/`。詳細は `CLAUDE.md`）。

## ビルド

```
go build -o stackchan-cli .
```

## テスト

実機、gateway、VOICEVOX は要らない。CI（GitHub Actions）は Go のテストを Windows と Linux で、Python のテストを Linux で実行する。

```
go test ./...
python -m unittest discover -s scripts -p "test_*.py"
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
./stackchan-cli pc                           # この PC の CPU・メモリ・ディスクの空き（デバイス不要、話さない）
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
- **夜は眠る**。`--sleep HH:MM-HH:MM`（既定 `00:00-07:00`、`--sleep off` で無効）の間は、本体の頭の
  タッチセンサーを止める（`set_touch_sensor_enabled`。本体自身のタッチ動作も止まる）。
  そして寝顔のアバターを送り、`~/.stackchan/reactions/sleep.txt`（既定: LED 消灯、画面の明るさ 10）を
  実行する。終わるとセンサーを戻し、通常のアバターを送って `wake.txt`（既定: 明るさ 100、標準の表情）を実行する。
  - 寝顔は `~/.stackchan/avatar-sleep/`（14 枚。表情とまばたきはすべて寝顔になる。口の形だけは発話に合わせて
    変わる）。`uv run --with pillow python scripts/gen_avatars.py --sleep` で作る。無ければ通常のアバターのまま眠る。
    本体がつながるたびにも、状態に合う方を送る（`--avatar=false` なら送らない）。
    手動では `stackchan-cli avatar-load --set sleep`（通常に戻すのは `avatar-load`）。
  - `stackchan-cli sleep` で時刻に関係なく眠り、`stackchan-cli wakeup` まで起きない（朝 7 時を過ぎても）。
    眠る時間帯に `wakeup` すると、その時間帯が終わるまで起きている。どちらも daemon が必要で、
    状態は daemon を再起動すると消える。数字付きの `sleep <ms>` は従来どおりスクリプト内の一時停止。
  - 本体がつながり直したとき、眠っていれば `sleep.txt` を実行し直す。起きていればセンサーを有効に
    するだけで、`wake.txt` は daemon の起動後に初めてつながったときだけ実行する（手で変えた明るさを
    再接続のたびに戻さないため）。
  - 本体の応答が遅れて失敗したら 30 秒ごとにやり直す（センサーは成功するまで、`sleep.txt` / `wake.txt`
    は最大 5 回。結果に `"error"` があればその行は失敗とみなす）。本体が離れている間は試さない。
  - 眠る瞬間に走っているタッチの反応は、終わってから `sleep.txt` を実行する。
  - センサーの設定は本体に保存される。眠っている間に daemon を止めると、次に daemon が起動して
    本体がつながるまでセンサーは止まったまま。画面タップの音声コマンドと IPC のコマンドは眠っている間も動く。
- 本体の画面をタップ → 話す → もう一度タップすると、**音声コマンド**として、daemon が聞き取った文を
  `~/.stackchan/voice-commands.txt` のキーワードと照合し、当たった行のコマンドを実行する
  （例: 「笑って」で `avatar happy`）。書式は `キーワード|別の書き方 => コマンド; コマンド`。
  上の行ほど優先で、`*` の行はどれにも当たらなかったときに実行する。daemon の初回起動時に雛形を生成し、
  保存すれば次の聞き取りから反映される。文字起こしの間は考えている顔（thinking）になる。
  - gateway が音声を `STACKCHAN_AUDIO_HOOK_URL`（daemon が `127.0.0.1:8771` で受ける）へ送り、
    daemon が gateway の Python に入っている faster-whisper で文字にする。daemon はモデルを読み込んだ
    Python を常駐させるので、文字起こしは CPU で約 2 秒、GPU で約 0.2 秒。
  - 短い発話は文脈が無く崩れやすい（「気温は」→「キヨンは」）ので、ルールのキーワードと「スタックチャン」を
    文字起こしの手がかり（Whisper の initial prompt）として渡す。ルールを足せば手がかりも増える
    （漢字の書き方だけ。ほかの語を含む組み合わせの語は省き、120 文字まで）。
    手がかりの語を 3 つ以上並べただけの文字起こしは、Whisper が手がかりをそのまま返したものとみなし、
    聞き取れなかった扱いにする。`STACKCHAN_STT_PROMPT=0` で手がかりを渡さない。
  - 聞き取った音声と文字起こしの結果を、直近 30 件まで `~/.stackchan/voice-samples/` に残す
    （`<時刻>.ogg` と `<時刻>.txt`。失敗は括弧書き。聞き取りの精度を実際のマイクの音で確かめるため。
    PC の外には出さない）。`STACKCHAN_VOICE_SAMPLES=0` で残さない。
  - 話さずに試すには `stackchan-cli voice "笑って"`。ルールの一覧は `stackchan-cli voice`。`--voice=false` で無効。
- daemon 稼働中は **one-shot も `repl` も `source` も**自動で IPC 転送される（その常駐 gateway を共有。
  `repl` は daemon があれば各行を転送、無ければ自前 gateway を起動）。転送したコマンドが失敗すると
  終了コードは 1 になる。
- daemon は起動時に **VOICEVOX エンジンをウィンドウ無しで自動起動**する（後述）。
- daemon のログ（stderr）は各行に時刻が付く。daemon が起動した gateway の出力は `~/.stackchan/gateway.log` に残る。
- daemon は、gateway が起動しないとき 10 秒おきに 3 回まで試し直し、gateway が途中で終了したら自分も終了する。
  どちらも終了コード 2 で終わるので、常駐させる側（タスクスケジューラーから起動するスクリプトなど）は 2 のときだけ
  起動し直せばよい。Windows のタスクの「失敗時に再起動」は、プログラムが 0 以外で終わっても働かない。
- gateway は mDNS の通知なし（`--no-mdns`）で起動する。本体の WebSocket Gateway URL を直接設定している前提。
  mDNS で本体に見つけさせたいときは `STACKCHAN_MDNS=1`。
- gateway・VOICEVOX エンジンなどの子プロセスは Windows の Job オブジェクトに入れてあり、
  daemon が強制終了されても一緒に終了する（ポート 8765/8766/50021 を掴んだまま残らない）。

### 天気

`say` の文に次のプレースホルダーを書くと、天気の文になる。末尾に `_tomorrow` を付けると明日の分
（例: `{weather_temp_tomorrow}`。明日は今の気温を読まない）。

| プレースホルダー | 例 |
|---|---|
| `{weather}` | 今日の東京は、くもり、ところにより雨。いまの気温は22度、最高27度。降水確率は30パーセント。 |
| `{weather_sky}` | 今日の東京は、くもり、ところにより雨。 |
| `{weather_temp}` | 今日の東京の気温は、いま22度、最高27度。 |
| `{weather_rain}` | 今日の東京の降水確率は30パーセント。 |

音声コマンドの既定のルールは「今日の天気は？」「天気だけ」「気温は？」「雨（傘）は？」と、
「明日の気温」「明日は雨？」に答え、ほかに「明日」を含む文には明日の天気全体を答える。全部を読むと 10 秒ほど
かかるので、短く聞きたいときは部分ごとのものを使う。`voice-commands.txt` が既にあるときは雛形で上書きしない
ので、天気のルールは手で足す（`stackchan-cli voice` で今のルールを、`voice.go` の `defaultVoiceCommands` で雛形を見られる）。

- 天気の文、最高・最低気温、降水確率は気象庁の予報 JSON（`https://www.jma.go.jp/bosai/forecast/data/forecast/<府県>.json`）
  から取る。今の気温は Open-Meteo から取る（取れなければ読み上げから外すだけ）。
- 予報は 30 分、今の気温は 10 分のあいだ取り直さない。
- 11 時と 17 時の発表には今日の最低気温が無いので、その間は読まない。17 時以降は「今夜の〜」になり、
  終わった日中の最高気温も読まない。降水確率は、まだ終わっていない 6 時間ごとの枠のうち最も高い値。
- 気象庁の天気の文は、VOICEVOX が読み違える語を次のように言い換えてから読む。
  - 「所により」→「、ところにより」 / 「平地」→「、へいち」（前の語にくっつかないよう区切る）
  - 「後」→「のち」 / 「一時」→「一時、」（「一時雨」が「いちじう」にならないよう区切る）
- 予報が取れないとき（通信の失敗、設定の誤り、形式の変更、明日の予報がまだ無い）は、そのプレースホルダーを
  「ごめんね、天気が取れなかった。」にして話し（同じ文のほかの部分はそのまま）、理由をログに出す。値を推測では埋めない。
- 地域は `~/.stackchan/weather.txt` に書く（住んでいる場所なのでリポジトリには入れない）。

  ```
  name = 東京        # 読み上げる地名。読み間違えるならひらがなで書く
  office = 130000    # 府県予報区
  area = 130010      # 天気と降水確率の地方
  station = 44132    # 気温の地点
  latitude = 35.69   # 今の気温（Open-Meteo）用。省くと今の気温を読まない
  longitude = 139.69
  ```

  - `office` と `area` は気象庁の `https://www.jma.go.jp/bosai/common/const/area.json` で引く。
    市区町村（`class20s`）の `parent` をたどった `class10s` のコードが `area`、その上の `offices` が `office`。
  - `station` は `forecast/<office>.json` の気温（3 つめの `timeSeries`）にある地点のコードから、近いものを選ぶ。
  - 緯度・経度は小数第 2 位（約 1km）までで十分（予報の格子は数 km 単位）。
- 出典: 気象庁の予報データ（[気象庁ホームページ](https://www.jma.go.jp/)）、
  今の気温は [Open-Meteo](https://open-meteo.com/)（CC BY 4.0）。

### PC の調子

`say` の文に次のプレースホルダーを書くと、この PC の CPU・メモリ・ディスクの空きを話す（Windows のみ。
ほかの OS では `{pc}` が「ごめんね、パソコンの様子が分からなかった。」、ほかは「不明」になる）。

| プレースホルダー | 例 |
|---|---|
| `{pc}` | 元気だよ。CPUは12パーセント、メモリは45パーセント、ディスクの空きは120ギガバイト。 |
| `{cpu}` / `{mem}` | 12（使用率の数字だけ。`say "CPUは{cpu}パーセント"` のように使う） |
| `{disk}` | 120ギガバイト（ドライブが複数なら「Cドライブが120ギガバイト、Dドライブが2テラバイト」） |

- `{pc}` は 3 段階で言い方を変え、気になる値（使用率の高い順）を先に言う。話す間の表情も変わる。
  - どれも `busy`（70%）未満 → 「元気だよ。」（happy）
  - CPU かメモリが `busy` 以上 → 「ちょっと忙しい。メモリが75パーセント。…」（thinking）
  - 警告の閾値を超えた値がある → 「ちょっと疲れてる。メモリが92パーセント。…」（sad）。ディスクは「空きが8ギガバイトしかない」
- CPU の使用率は、daemon があれば直近 1〜2 分の平均（daemon が 1 分ごとに記録している）、無ければ 1 秒間の測定。
- ディスクの空きはエクスプローラーと同じ単位（1 ギガバイト = 1024³ バイト）で読む。
- 音声コマンドの既定のルールは「調子どう？」「元気？」に `{pc}` で答える。`voice-commands.txt` が既にあるときは
  手で足す: `調子どう|ちょうしどう|調子は|ちょうしは|元気|げんき => say "{pc}"`
- `stackchan-cli pc` で、話さずに値と `{pc}` の文を確かめられる。

daemon は、値が閾値を超えていたら話して知らせる（例: 「メモリが92パーセントだよ。何か閉じたほうがいいかも。」）。

- 確認は `interval`（既定 60 分）ごと。daemon の起動から 5 分後が最初。CPU は 5 分間の平均で判断する。
- 一度知らせた項目は、閾値を下回るまで黙る。
- 眠っている間（`--sleep` の時間帯と `sleep` コマンドの間）と `quiet` の時間帯は話さず、次の確認に回す。
  本体がつながっていないとき、発話に失敗したときも同じ。
- 確認の結果は daemon.log に `pc check:` の行で出る。

設定は `~/.stackchan/pc-monitor.txt`（daemon の初回起動で雛形を作る。無ければ初期値で動く）。
使うたびに読み直すので、再起動は要らない。

```
cpu = 90         # CPU の使用率（5 分の平均）がこの % 以上で警告する
mem = 90         # メモリの使用率がこの % 以上で警告する
disk_free = 10   # ディスクの空きがこの % 未満で警告する
disk_free_gb = 0 # 1 以上なら、% の代わりに空きがこの GB 未満で警告する
busy = 70        # 「調子どう」で、CPU かメモリがこの % 以上なら「ちょっと忙しい」と言う
interval = 60    # 警告の確認の間隔（分）。0 で警告しない
quiet = off      # 警告を話さない時間帯（HH:MM-HH:MM）。眠っている間はいつも話さない
drives = C:      # 見るドライブ（, 区切り。例: C:, D:）
```

### Claude に尋ねる（ask）

```
stackchan-cli ask "東京タワーってなに？"      # Claude に尋ねて、答えを話す（待つ間は thinking の顔）
stackchan-cli ask --quiet "こんにちは"         # 答えを表示するだけ（本体を使わない）
```

- この PC の Claude Code（`claude -p`）を非対話で起動して尋ねる。API キーは使わず、Claude Code にログインした
  アカウントの利用枠で動く。
- 速さのため、Claude Code の既定のシステムプロンプトを短いロボット用の指示（`~/.stackchan/ask/system.txt`）に置き換え、
  使えるツールを Web 検索（WebSearch / WebFetch）だけにし、MCP サーバーと個人の設定（CLAUDE.md など）を読み込まない。
  sonnet で返事まで 3〜6 秒（2026-10-20 に測定）。
- 前の答えから 5 分以内の質問は、同じ会話として続ける（「それっていつできたの？」のような聞き返しができる）。
- 1 日 50 回まで。超えると「今日はもう調べられないよ」と話す。30 秒で答えが無ければ「時間内に答えられなかった」と話す。
- 設定は `~/.stackchan/ask.txt`（daemon の初回起動で雛形を作る）:

  ```
  model = sonnet   # claude -p の --model
  limit = 50       # 1 日に Claude を呼ぶ回数の上限（0 で上限なし）
  timeout = 30     # 返事を待つ秒数
  memory = 5       # 前の質問からこの分数以内なら、同じ会話として続ける（0 で毎回新しく）
  config_dir =     # claude の設定ディレクトリ（CLAUDE_CONFIG_DIR）。空なら daemon の環境のまま
  ```

  Claude Code の設定ディレクトリを使い分けている（シェルの関数などで `CLAUDE_CONFIG_DIR` を切り替えている）なら、
  daemon は `claude` をその関数を通さずに起動するので、`config_dir` で使うディレクトリを指定する。
- 音声コマンドの既定のルール: 「〜ってなに」「調べて」「教えて」を含む話しかけを `ask "{heard}"` で Claude に尋ねる
  （`{heard}` は聞き取った文）。「〜ってなに」はタイマーやポモドーロのルールより先に見るので、「ポモドーロってなに？」で
  ポモドーロが始まることはない。天気など組み込みで答えられるもの（「明日の天気を教えて」）は、上のルールが先に答える。どのルールにも当たらない
  話しかけ（`*`）は Claude に回さない（聞き間違いや雑音で呼ばないため）。
- `claude` は PATH から探す。`STACKCHAN_CLAUDE_EXE` で場所を指定できる。

### タイマーとポモドーロ

daemon がタイマーを持つ（daemon が無いと使えない。daemon を再起動すると消える）。

```
stackchan-cli timer 3m          # 3 分のタイマー（1h30m、90s、3分 とも書ける）。時間が来たら「3分たったよ」と話す
stackchan-cli timer             # 動いているタイマーとポモドーロ（--say で読み上げる）
stackchan-cli timer cancel      # タイマーを全部止める（ポモドーロは止めない）
stackchan-cli pomodoro start    # 作業 25 分 → 休憩 5 分 →（4 回目の休憩は 15 分）… を繰り返す
stackchan-cli pomodoro ok       # 作業の終わり・休憩の終わりに応える（休憩・次の作業を始める）
stackchan-cli pomodoro stop     # ポモドーロを止める
```

- 作業中と休憩中は、12 個の LED が残り時間の目盛りになる（暗めに点け、時間とともに減る）。作業の残りが
  10 分で黄色、5 分で橙色になる（声は出さない控えめな知らせ）。休憩中は緑。
- 作業が終わると、LED を赤くし、首を振って「休憩しよう」と話す。頭をタッチするか「はい」と言うと休憩が始まる。
  応えるまで LED は赤のままで、3 分ごとに 5 回まで呼びかける。応答を待っている間の頭のタッチは、いつもの反応の
  代わりに応答になる。
- 休憩が終わると同じように知らせ（LED は緑）、応答（頭のタッチか「はい」）で次の作業を始める。呼びかけの
  繰り返しも作業の終わりと同じ。
- 長さは `pomodoro start --work 25m --break 5m --long 15m --every 4` で変えられる（`--every 0` で長い休憩なし）。
  開始・取り消しなどの発話は `--quiet` で止められる。
- 眠っている間も、時間が来たら鳴らす（目盛りは眠っている間は描かず、起きたら描き直す。応答の呼びかけの繰り返しは
  眠っている間は止め、起きたら続ける）。
- 知らせの中身（色、首振り、セリフ）は `~/.stackchan/reactions/` のスクリプトで変えられる（daemon の初回起動で作る）:
  `timer-done.txt`、`pomodoro-work-done.txt`、`pomodoro-remind.txt`、`pomodoro-break-start.txt`、
  `pomodoro-break-done.txt`、`pomodoro-break-remind.txt`、`pomodoro-work-start.txt`。文の `{timer}`（タイマーの長さ）、
  `{round}`（何回目の作業か）、`{break}`（休憩の長さ）、`{work}`（作業の長さ）は、話す前にその値になる。
- 応答を待っている間は、「はい」「OK」「わかった」「了解です」などの短い返事だけの発話が応答になる（ルールより先に見る。待っていない
  ときの「はい」はルールのとおりで、当たらなければ「わからなかった」と答える）。
- 音声コマンドの既定のルール: 「3分タイマー」「三分のタイマー」「1時間半タイマー」、「タイマー止めて」、
  「ポモドーロ始めるよ」「ポモドーロ終わり」、「あと何分？」。ルールのキーワードの `{dur}` は時間に当たり、
  コマンドの `{dur}` がその時間になる（`{dur}タイマー => timer {dur}`）。数は算用数字でも漢数字でもよい。
  `voice-commands.txt` が既にあるときは、`voice.go` の `defaultVoiceCommands` から手で足す。

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
  話している間の表情は `say --while-face <name>` で指定する（省くと変えない。`{pc}` を含む文はその表情）。
  文の中の `{date}` `{weekday}` `{time}` は、話すときの日付・曜日・時刻（例: `2026年10月17日、土曜日、22時40分`）になる。
- `embarrassed` を標準の表情にしている。発話のあと、音声コマンドの文字起こしのあとに戻す表情で、
  daemon は本体がつながるたび、この表情へ切り替える（本体の起動時の表情は `idle`。眠っている間は切り替えない）。
- 本体の画面は、操作が無いと 5 分後に消える。止めるには
  `stackchan-cli call set_off_timeout --json '{"seconds":0}'`（本体に保存され、再起動後も有効。戻すときは 300）。
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
