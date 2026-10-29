param([string]$Version='dev',[string]$Output='dist')
$ErrorActionPreference='Stop'
if($Version -notmatch '^[A-Za-z0-9._-]+$'){throw 'Invalid release version'}
$repo=Split-Path $PSScriptRoot -Parent
if([IO.Path]::IsPathRooted($Output)){$releaseOutput=[IO.Path]::GetFullPath($Output)}else{$releaseOutput=[IO.Path]::GetFullPath((Join-Path $repo $Output))}
New-Item -ItemType Directory -Force -Path $releaseOutput | Out-Null
$revision=(& git -c "safe.directory=$repo" -C $repo rev-parse --short=12 HEAD).Trim()
if($LASTEXITCODE -ne 0){throw 'Cannot read Git revision'}
$dirty=& git -c "safe.directory=$repo" -C $repo status --porcelain --untracked-files=normal
if($LASTEXITCODE -ne 0){throw 'Cannot inspect worktree'}
if($dirty){$revision+='+working'}
$releaseEpoch=(& git -c "safe.directory=$repo" -C $repo show -s --format=%ct HEAD).Trim()
if($LASTEXITCODE -ne 0){throw 'Cannot read commit timestamp'}
$oldOS=$env:GOOS;$oldArch=$env:GOARCH;$oldCgo=$env:CGO_ENABLED;$oldEpoch=$env:SOURCE_DATE_EPOCH
try {
  $env:CGO_ENABLED='0'
  $env:SOURCE_DATE_EPOCH=$releaseEpoch
  foreach($target in @(@('linux','amd64'),@('linux','arm64'),@('windows','amd64'))){
    $env:GOOS=$target[0];$env:GOARCH=$target[1]
    $name='cyapi-'+$target[0]+'-'+$target[1]
    if($target[0] -eq 'windows'){$name+='.exe'}
    Push-Location $repo
    try{& go build -buildvcs=false -trimpath -ldflags "-s -w -X main.version=$Version -X main.commit=$revision" -o (Join-Path $releaseOutput $name) ./cmd/cyapi
      if($LASTEXITCODE -ne 0){throw "Build failed: $name"}}finally{Pop-Location}
  }
  $releasePython=Get-Command python -ErrorAction SilentlyContinue
  if(!$releasePython){$releasePython=Get-Command python3 -ErrorAction Stop}
  & $releasePython.Source (Join-Path $repo 'scripts/package-release.py') $releaseOutput
  if($LASTEXITCODE -ne 0){throw 'Packaging failed'}
}finally{$env:GOOS=$oldOS;$env:GOARCH=$oldArch;$env:CGO_ENABLED=$oldCgo;$env:SOURCE_DATE_EPOCH=$oldEpoch}
