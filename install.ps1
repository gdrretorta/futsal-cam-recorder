# Futsal Cam Recorder - installer / upgrader
#
# Run in PowerShell as Administrator:
#   irm https://raw.githubusercontent.com/gdrretorta/futsal-cam-recorder/development/install.ps1 | iex
#
# - Upgrading (including from 1.x): installs into the folder of the existing
#   service, keeping settings (prod.env is converted to config.json) and recordings.
# - New install: C:\FutsalCamRecorder, or the folder in $env:SRR_INSTALL_DIR.
#
# After this, the app updates itself; this script is only needed once per PC.

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$Repo = 'gdrretorta/futsal-cam-recorder'
# The service keeps its 1.x name, so upgrades find and replace it.
$ServiceName = 'SimpleRTSPRecorder'
$ExeName = 'futsal-cam-recorder.exe'
$ZipName = 'futsal-cam-recorder-win-x64.zip'
# Name of the exe until 2.8 (the app was called Simple RTSP Recorder).
$LegacyExeName = 'simple-rtsp-recorder.exe'

function Step($msg) { Write-Host "==> $msg" -ForegroundColor Cyan }

function Read-SecretText($prompt) {
    $secure = Read-Host "    $prompt" -AsSecureString
    $bstr = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($secure)
    try { return [Runtime.InteropServices.Marshal]::PtrToStringBSTR($bstr) }
    finally { [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($bstr) }
}

$principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'Execute o PowerShell como Administrador e tente novamente.'
}

# Where to install: next to the existing service, if any.
$existing = Get-CimInstance Win32_Service -Filter "Name='$ServiceName'" -ErrorAction SilentlyContinue
if ($existing) {
    $path = $existing.PathName.Trim()
    if ($path.StartsWith('"')) { $path = $path.Substring(1, $path.IndexOf('"', 1) - 1) }
    else { $path = ($path -split '\.exe', 2)[0] + '.exe' }
    $InstallDir = Split-Path $path -Parent
} elseif ($env:SRR_INSTALL_DIR) {
    $InstallDir = $env:SRR_INSTALL_DIR
} else {
    $InstallDir = 'C:\FutsalCamRecorder'
}
Step "Pasta de instalação: $InstallDir"

Step 'A obter a versão mais recente...'
$release = Invoke-RestMethod "https://api.github.com/repos/$Repo/releases/latest"
$asset = $release.assets | Where-Object { $_.name -eq $ZipName } | Select-Object -First 1
if (-not $asset) { throw "A versão $($release.tag_name) não tem o ficheiro $ZipName." }
Write-Host "    $($release.tag_name)"

$tmp = Join-Path $env:TEMP ("srr-" + [guid]::NewGuid())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    Step 'A descarregar...'
    $zip = Join-Path $tmp $ZipName
    Invoke-WebRequest -Uri $asset.browser_download_url -OutFile $zip -UseBasicParsing
    Expand-Archive -Path $zip -DestinationPath (Join-Path $tmp 'pkg')

    if ($existing -and $existing.State -ne 'Stopped') {
        Step 'A parar o serviço atual...'
        Stop-Service -Name $ServiceName -Force
        (Get-Service -Name $ServiceName).WaitForStatus('Stopped', [TimeSpan]::FromSeconds(30))
    }
    # The 1.x wrapper may leave the old process behind for a moment.
    Get-Process -Name 'futsal-cam-recorder', 'simple-rtsp-recorder' -ErrorAction SilentlyContinue | Stop-Process -Force
    Start-Sleep -Seconds 2

    Step 'A copiar ficheiros...'
    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    Copy-Item -Path (Join-Path $tmp 'pkg\*') -Destination $InstallDir -Recurse -Force

    $exe = Join-Path $InstallDir $ExeName
    # Upgrades from 2.6 or older turn the admin password into an admin user
    # on their own, so this only asks on a new install.
    & $exe has-admin-password
    if ($LASTEXITCODE -ne 0) {
        Step 'Crie a conta de administrador (para entrar na interface web)'
        while ($true) {
            $email = (Read-Host '    Email').Trim()
            if ($email -match '^[^@\s]+@[^@\s]+\.[^@\s]+$') { break }
            Write-Host '    Email inválido.' -ForegroundColor Yellow
        }
        while ($true) {
            $first = Read-SecretText 'Palavra-passe (mínimo 8 caracteres)'
            $second = Read-SecretText 'Repita a palavra-passe'
            if ($first -ne $second) { Write-Host '    As palavras-passe não coincidem.' -ForegroundColor Yellow; continue }
            if ($first.Length -lt 8) { Write-Host '    Tem de ter pelo menos 8 caracteres.' -ForegroundColor Yellow; continue }
            break
        }
        # Passed on stdin (UTF-8) so it never shows up in the process list.
        $previousEncoding = $OutputEncoding
        $OutputEncoding = New-Object Text.UTF8Encoding $false
        $first | & $exe add-user $email admin
        $OutputEncoding = $previousEncoding
        if ($LASTEXITCODE -ne 0) { throw 'Falha ao criar a conta de administrador.' }
    }

    Step 'A instalar o serviço...'
    & $exe install
    if ($LASTEXITCODE -ne 0) { throw 'Falha ao instalar o serviço (ver mensagem acima).' }

    # The service now runs the exe with the new name: remove the old one.
    $legacyExe = Join-Path $InstallDir $LegacyExeName
    if (Test-Path $legacyExe) {
        Remove-Item $legacyExe, "$legacyExe.old", "$legacyExe.new" -Force -ErrorAction SilentlyContinue
    }
} finally {
    Remove-Item $tmp -Recurse -Force -ErrorAction SilentlyContinue
}

Write-Host ''
Write-Host 'Instalação concluída.' -ForegroundColor Green
Write-Host "Interface: http://localhost:8080   (entre com o email e a palavra-passe de administrador)"
Write-Host "Pasta:     $InstallDir"
Write-Host 'As próximas versões são instaladas automaticamente quando não houver gravações.'
