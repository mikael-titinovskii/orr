$ErrorActionPreference = "Stop"

$RepositoryUrl = if ($env:ORR_REPOSITORY_URL) { $env:ORR_REPOSITORY_URL } else { "https://github.com/mikael-titinovskii/orr.git" }
$MinimumGo = [Version]"1.23"

if (-not (Get-Command git -ErrorAction SilentlyContinue)) {
    throw "Git is required. Install Git and run this script again."
}
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    throw "Go 1.23 or newer is required. Download it from https://go.dev/dl/."
}

$GoVersionText = (& go env GOVERSION).Trim()
if ($GoVersionText -notmatch '^go(\d+\.\d+)(?:\.\d+)?') {
    throw "Could not understand installed Go version: $GoVersionText"
}
$GoVersion = [Version]$Matches[1]
if ($GoVersion -lt $MinimumGo) {
    throw "Go 1.23 or newer is required; found $GoVersionText. Download it from https://go.dev/dl/."
}

$OpenRouterKeyPattern = '^sk-or-v1-[0-9a-f]{64}$'
$KeyFormatMessage = "OPENROUTER_API_KEY must be 'sk-or-v1-' followed by exactly 64 lowercase hexadecimal characters."

$OpenRouterKey = ""
if ($env:ORR_OPENROUTER_API_KEY) {
    if ($env:ORR_OPENROUTER_API_KEY -cnotmatch $OpenRouterKeyPattern) {
        throw $KeyFormatMessage
    }
    $OpenRouterKey = $env:ORR_OPENROUTER_API_KEY
} else {
    while ($OpenRouterKey -cnotmatch $OpenRouterKeyPattern) {
        $SecureKey = Read-Host "OPENROUTER_API_KEY (required)" -AsSecureString
        $KeyPointer = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($SecureKey)
        try {
            $OpenRouterKey = [Runtime.InteropServices.Marshal]::PtrToStringBSTR($KeyPointer)
        } finally {
            [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($KeyPointer)
        }
        if ($OpenRouterKey -cnotmatch $OpenRouterKeyPattern) {
            Write-Warning $KeyFormatMessage
        }
    }
}
$ConfigDir = Join-Path ([Environment]::GetFolderPath('ApplicationData')) "orr"
$BinDir = Join-Path $env:LOCALAPPDATA "Programs\orr"
$SourceDir = $null
$TempDir = $null

if ($PSScriptRoot -and
    (Test-Path -LiteralPath (Join-Path $PSScriptRoot "go.mod")) -and
    (Test-Path -LiteralPath (Join-Path $PSScriptRoot "cmd\orr") -PathType Container)) {
    $SourceDir = (Resolve-Path -LiteralPath $PSScriptRoot).Path
    Write-Host "Installing from $SourceDir..."
} else {
    $TempDir = Join-Path ([IO.Path]::GetTempPath()) ("orr-install-" + [guid]::NewGuid())
    Write-Host "Downloading orr..."
    New-Item -ItemType Directory -Force -Path $TempDir | Out-Null
    & git clone --depth 1 --quiet $RepositoryUrl (Join-Path $TempDir "source")
    if ($LASTEXITCODE -ne 0) { throw "Could not download $RepositoryUrl" }
    $SourceDir = Join-Path $TempDir "source"
}

try {
    New-Item -ItemType Directory -Force -Path $ConfigDir, $BinDir | Out-Null
    $EnvPath = Join-Path $ConfigDir ".env"
    if (-not (Test-Path -LiteralPath $EnvPath)) {
        Copy-Item -LiteralPath (Join-Path $SourceDir ".env.example") -Destination $EnvPath
    }
    $EnvLines = @(Get-Content -LiteralPath $EnvPath)
    $FoundKey = $false
    $UpdatedLines = foreach ($Line in $EnvLines) {
        if ($Line -match '^OPENROUTER_API_KEY=') {
            $FoundKey = $true
            "OPENROUTER_API_KEY=$OpenRouterKey"
        } else {
            $Line
        }
    }
    if (-not $FoundKey) { $UpdatedLines += "OPENROUTER_API_KEY=$OpenRouterKey" }
    [IO.File]::WriteAllLines($EnvPath, [string[]]$UpdatedLines, [Text.UTF8Encoding]::new($false))

    Write-Host "Building orr with $GoVersionText..."
    Push-Location $SourceDir
    try {
        & go build -trimpath -o (Join-Path $BinDir "orr.exe") ./cmd/orr
        if ($LASTEXITCODE -ne 0) { throw "The orr build failed." }
    } finally {
        Pop-Location
    }

    $UserPath = [Environment]::GetEnvironmentVariable("Path", "User")
    $PathParts = @($UserPath -split ';' | Where-Object { $_ })
    if ($PathParts -notcontains $BinDir) {
        $NewPath = (($PathParts + $BinDir) -join ';')
        [Environment]::SetEnvironmentVariable("Path", $NewPath, "User")
        Write-Host "Added $BinDir to your user PATH. Open a new terminal after installation."
    }
    $env:Path = "$BinDir;$env:Path"

    Write-Host "Checking Kimi Code and OpenCode configuration..."
    & (Join-Path $BinDir "orr.exe") integrate --env $EnvPath

    Write-Host "`nInstalled orr to $(Join-Path $BinDir 'orr.exe')"
    Write-Host "Configuration: $ConfigDir"
    Write-Host "Start it with: orr serve"
    Write-Host "Tab completion: run 'orr completion' for setup instructions."
} finally {
    if ($TempDir -and (Test-Path -LiteralPath $TempDir)) {
        Remove-Item -LiteralPath $TempDir -Recurse -Force
    }
}
