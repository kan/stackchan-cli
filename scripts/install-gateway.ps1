# Install (or reinstall) the stackchan-mcp gateway at a pinned version, with
# everything this CLI needs in the same uv tool environment:
#   - [tts] (opuslib) and [stt-faster-whisper]
#   - av<19 (PyAV 19 rejects the metadata_errors argument faster-whisper passes)
#   - the CUDA libraries for faster-whisper on an NVIDIA GPU
# `uv tool install --force` rebuilds the environment, so all of these are
# installed together every time.
#
#   pwsh scripts/install-gateway.ps1                 # default version, GPU if nvidia-smi finds one
#   pwsh scripts/install-gateway.ps1 -Version 0.21.0
#   pwsh scripts/install-gateway.ps1 -Cpu            # skip the CUDA libraries (about 1GB)
#
# Stop the daemon first: a running gateway holds files in the environment.
param(
    [string]$Version = "0.20.0",
    [switch]$Cpu
)
$ErrorActionPreference = "Stop"

$toolDir = Join-Path $env:APPDATA "uv\tools\stackchan-mcp"
# The gateway, and Pythons from its environment (the daemon's speech
# recognizer), hold files that the reinstall deletes.
$busy = Get-Process -ErrorAction SilentlyContinue | Where-Object {
    $_.Name -eq "stackchan-mcp" -or ($_.Path -and $_.Path.StartsWith($toolDir, [StringComparison]::OrdinalIgnoreCase))
}
if ($busy) {
    $list = ($busy | ForEach-Object { "$($_.Name) ($($_.Id))" }) -join ", "
    Write-Error "still running: $list. Stop the daemon first; see docs/upstream-upgrade.md."
}

$gpu = -not $Cpu -and [bool](Get-Command nvidia-smi -ErrorAction SilentlyContinue)
$uvArgs = @(
    "tool", "install", "--force", "--python", "3.12",
    "stackchan-mcp[tts,stt-faster-whisper]==$Version",
    "--with", "av<19"
)
if ($gpu) {
    $uvArgs += @("--with", "nvidia-cublas-cu12", "--with", "nvidia-cudnn-cu12==9.*")
}
Write-Host "uv $($uvArgs -join ' ')"
& uv @uvArgs
if ($LASTEXITCODE -ne 0) { Write-Error "uv tool install failed ($LASTEXITCODE)" }

# Check what the CLI relies on.
$installed = (& stackchan-mcp --version) 2>&1
Write-Host "gateway: $installed"
if ("$installed" -notmatch "(^|\s)$([regex]::Escape($Version))(\s|$)") { Write-Error "expected version $Version" }

$python = Join-Path $toolDir "Scripts\python.exe"
# From a file, not -c: an older pwsh strips the quotes inside a -c argument.
$checkFile = Join-Path ([IO.Path]::GetTempPath()) "stackchan-gateway-check.py"
Set-Content -Path $checkFile -Encoding utf8 -Value @'
import importlib.metadata as m, sys
av = m.version("av")
print("av", av, "faster-whisper", m.version("faster-whisper"), "opuslib", m.version("opuslib"))
if int(av.split(".")[0]) >= 19:
    sys.exit("av must be < 19")
for p in ("nvidia-cublas-cu12", "nvidia-cudnn-cu12"):
    try:
        print(p, m.version(p))
    except m.PackageNotFoundError:
        print(p, "not installed (speech recognition runs on the CPU)")
'@
& $python -I $checkFile
$checkExit = $LASTEXITCODE
Remove-Item $checkFile -ErrorAction SilentlyContinue
if ($checkExit -ne 0) { Write-Error "environment check failed" }

# opuslib finds opus.dll on PATH; uv does not install it (docs/tts-voicevox.md).
$opus = Join-Path $env:USERPROFILE ".local\bin\opus.dll"
if (Test-Path $opus) {
    Write-Host "opus.dll: $opus"
} else {
    Write-Warning "opus.dll is missing from ~/.local/bin; say will fail. See docs/tts-voicevox.md."
}
