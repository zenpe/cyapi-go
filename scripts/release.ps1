param([string]$Version='dev',[string]$Output='dist')
$ErrorActionPreference='Stop'
$repo=Split-Path $PSScriptRoot -Parent
$releaseOutput=[IO.Path]::GetFullPath((Join-Path $repo $Output))
New-Item -ItemType Directory -Force -Path $releaseOutput | Out-Null
$revision=(& git -C $repo rev-parse --short HEAD).Trim()
if($LASTEXITCODE -ne 0){throw 'Cannot read Git revision'}
$oldOS=$env:GOOS;$oldArch=$env:GOARCH;$oldCgo=$env:CGO_ENABLED
try {
  $env:CGO_ENABLED='0'
  foreach($target in @(@('linux','amd64'),@('linux','arm64'),@('windows','amd64'))){
    $env:GOOS=$target[0];$env:GOARCH=$target[1]
    $name='cyapi-'+$target[0]+'-'+$target[1]
    if($target[0] -eq 'windows'){$name+='.exe'}
    Push-Location $repo
    try{& go build -buildvcs=false -trimpath -ldflags "-s -w -X main.version=$Version -X main.commit=$revision" -o (Join-Path $releaseOutput $name) ./cmd/cyapi
      if($LASTEXITCODE -ne 0){throw "Build failed: $name"}}finally{Pop-Location}
  }
  Get-ChildItem -LiteralPath $releaseOutput -File -Filter 'cyapi-*' | ForEach-Object{
    $digest=(Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash.ToLowerInvariant()
    "$digest  $($_.Name)"
  } | Set-Content -LiteralPath (Join-Path $releaseOutput 'SHA256SUMS') -Encoding ascii
}finally{$env:GOOS=$oldOS;$env:GOARCH=$oldArch;$env:CGO_ENABLED=$oldCgo}
