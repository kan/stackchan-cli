# TTS（しゃべらせる）セットアップ — VOICEVOX

`say "テキスト"` は **gateway が VOICEVOX で音声合成 → Opus 圧縮 → WebSocket で
デバイスへ送出 → スピーカー再生**する（ファーム変更不要）。

## 必要なもの

### 1. VOICEVOX エンジン（外部・:50021）
- [VOICEVOX](https://voicevox.hiroshiba.jp/) の Windows 版を入れておく。GUI を起動する必要はない。
- **CLI がエンジンを自動起動する**: エンジンが応答しないとき、`vv-engine\run.exe` を
  **ウィンドウ無しの子プロセス**として起動し、準備完了（約4秒）を待ってから発話する。
  - daemon は起動時に立ち上げて保持し、daemon の終了と一緒に止める。
  - daemon 無しの one-shot `say` は、その都度起動して終了時に止める。
  - GUI などで既にエンジンが動いていれば、それをそのまま使う。
  - エンジンのログは `~/.stackchan/voicevox.log`。
- 関連する環境変数:

  | 変数 | 意味 |
  |---|---|
  | `STACKCHAN_VOICEVOX_URL` | エンジンの URL（既定 `http://127.0.0.1:50021`）。localhost 以外なら自動起動しない |
  | `STACKCHAN_VOICEVOX_ENGINE` | `run.exe` のパス（既定は `%ProgramFiles%\VOICEVOX\vv-engine\run.exe`、次に `%LOCALAPPDATA%\Programs\VOICEVOX\vv-engine\run.exe`） |
  | `STACKCHAN_VOICEVOX_AUTOSTART` | `0` で自動起動しない |

### 2. gateway に opuslib（Opus エンコード）
gateway は VOICEVOX エンジン実装を内蔵。Opus 化用の `opuslib` は `[tts]` extra に含まれる
（聞き取りも使うので `[stt-faster-whisper]` と、その不具合回避の `av<19` も一緒に入れる。README 参照）：
```powershell
uv tool install --force --python 3.12 "stackchan-mcp[tts,stt-faster-whisper]==0.18.0" --with "av<19"
```

### 3. Windows 用 libopus（`opus.dll`）を PATH に置く
`opuslib` は実行時に `find_library('opus')` で `opus.dll` を PATH から探す。Windows には
無いので、PyPI の `PyOgg` 同梱の libopus を流用するのが手軽：
```powershell
# PyOgg の opus.dll を PATH 上の ~/.local/bin にコピー
uv run --with pyogg python -c "import pyogg,os,shutil; d=os.path.dirname(pyogg.__file__); shutil.copy(os.path.join(d,'opus.dll'), os.path.join(os.path.expanduser('~'),'.local','bin','opus.dll')); print('ok')"
```
確認：
```powershell
uv tool run --from stackchan-mcp --with opuslib python -c "import opuslib; e=opuslib.Encoder(16000,1,opuslib.APPLICATION_VOIP); print('OPUS OK', len(e.encode(b'\x00\x00'*320,320)))"
```
`OPUS OK ...` が出れば OK。`~/.local/bin` がユーザー PATH にあること（uv tool が登録済み）。

## 話者

- VOICEVOX 稼働中に話者一覧を取得：`Invoke-RestMethod http://127.0.0.1:50021/speakers`
- 例: ずんだもん=3 / 春日部つむぎ=8 / 四国めたん=2 / 九州そら=16 など（ノーマル style の id）。

## 使い方

```powershell
stackchan-cli say "こんにちは"            # 既定話者（この CLI は 8=春日部つむぎ）
stackchan-cli say --speaker 3 "こんにちは"  # 話者を一時変更（フラグは text の前に）
stackchan-cli say --face "" "顔を変えない"   # 発話後の表情変更をしない
```
- 既定話者は CLI が `STACKCHAN_VOICEVOX_DEFAULT_SPEAKER=8`（春日部つむぎ）を注入。
  変えたい時は環境変数で上書き、または `say --speaker N`。
- **発話後は自動で `set_avatar embarrassed`**（`--face <name>` で変更、`--face ""` で無効）。
- `say` は再生が終わるまでブロック（フレームを実時間ペースで送出）。終了時に gateway が
  `tts stop` を送り、口パクが止まる（CLI のグレースフル終了で確実に届く）。

## 仕組み / 注意
- 条件: **VOICEVOX が使える（自動起動を含む）+ デバイス接続中**。
- LGPL の VOICEVOX は別プロセス（HTTP）として動かす（gateway は HTTP 越しに呼ぶだけ）。
- 音量は `volume <0-100>`、画面輝度は `brightness <0-100>` で調整可。
