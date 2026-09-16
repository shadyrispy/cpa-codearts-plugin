@echo off
setlocal EnableExtensions DisableDelayedExpansion

rem Publish this repository and trigger .github/workflows/release.yml.
rem
rem Usage:
rem   release.bat
rem   release.bat 0.1.1
rem   release.bat 0.1.1 --yes
rem   release.bat 0.1.1 --dry-run

cd /d "%~dp0"

where git.exe >nul 2>nul
if errorlevel 1 (
  echo ERROR: git.exe was not found in PATH.
  goto :abort_release
)

if not exist "main.go" (
  echo ERROR: main.go was not found. Run this script from the repository.
  goto :abort_release
)
if not exist ".github\workflows\release.yml" (
  echo ERROR: .github\workflows\release.yml was not found.
  goto :abort_release
)
if not exist "registry.json" (
  echo ERROR: registry.json was not found.
  goto :abort_release
)

set "PLUGIN_VERSION="
for /f "tokens=4" %%V in ('findstr /B /C:"const pluginVersion = " main.go') do set "PLUGIN_VERSION=%%~V"
if not defined PLUGIN_VERSION (
  echo ERROR: Could not read pluginVersion from main.go.
  goto :abort_release
)

set "VERSION=%~1"
if not defined VERSION set "VERSION=%PLUGIN_VERSION%"
if /i "%VERSION:~0,1%"=="v" set "VERSION=%VERSION:~1%"

echo %VERSION%| findstr /R /X "[0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*" >nul
if errorlevel 1 (
  echo ERROR: Version must use dotted numeric form, for example 0.1.1.
  goto :abort_release
)
if not "%VERSION%"=="%PLUGIN_VERSION%" (
  echo ERROR: Requested version %VERSION% does not match main.go %PLUGIN_VERSION%.
  goto :abort_release
)

powershell.exe -NoProfile -Command "$j = Get-Content -Raw -LiteralPath 'registry-entry.json' | ConvertFrom-Json; if ($j.version -ne '%VERSION%') { exit 1 }"
if errorlevel 1 (
  echo ERROR: registry-entry.json does not contain version %VERSION%.
  goto :abort_release
)
powershell.exe -NoProfile -Command "$j = Get-Content -Raw -LiteralPath 'registry.json' | ConvertFrom-Json; if ($j.plugins[0].version -ne '%VERSION%') { exit 1 }"
if errorlevel 1 (
  echo ERROR: registry.json does not contain version %VERSION%.
  goto :abort_release
)

for /f "delims=" %%B in ('git branch --show-current') do set "BRANCH=%%B"
if /i not "%BRANCH%"=="master" (
  echo ERROR: Current branch is "%BRANCH%"; release publishing requires master.
  goto :abort_release
)

git remote get-url origin >nul 2>nul
if errorlevel 1 (
  echo ERROR: Git remote "origin" is not configured.
  goto :abort_release
)

git rev-parse -q --verify "refs/tags/v%VERSION%" >nul 2>nul
if not errorlevel 1 (
  echo ERROR: Local tag v%VERSION% already exists. Bump the version before publishing again.
  goto :abort_release
)
git ls-remote --exit-code --tags origin "refs/tags/v%VERSION%" >nul 2>nul
if not errorlevel 1 (
  echo ERROR: Remote tag v%VERSION% already exists. Bump the version before publishing again.
  goto :abort_release
)

echo.
echo Release v%VERSION%
echo   1. Stage all repository changes
echo   2. Commit as "release: v%VERSION%"
echo   3. Push master to origin
echo   4. Create and push annotated tag v%VERSION%
echo   5. GitHub Actions builds and publishes the Release
echo.
git status --short
echo.

if /i "%~2"=="--dry-run" (
  echo DRY RUN: validation passed; no files, commits, tags, or remotes were changed.
  goto :success
)

if /i "%~2"=="--yes" goto :publish
set "ANSWER="
set /p "ANSWER=Continue publishing v%VERSION%? [y/N]: "
if /i "%ANSWER%"=="y" goto :publish
echo Cancelled.
exit /b 0

:publish

git add -A
if errorlevel 1 goto :git_failed

git diff --cached --quiet
if errorlevel 1 (
  git commit -m "release: v%VERSION%"
  if errorlevel 1 goto :git_failed
) else (
  echo No uncommitted changes; releasing the current HEAD.
)

git push origin master
if errorlevel 1 goto :git_failed

git tag -a "v%VERSION%" -m "Release v%VERSION%"
if errorlevel 1 goto :git_failed

git push origin "v%VERSION%"
if errorlevel 1 goto :tag_push_failed

echo.
echo Published tag v%VERSION%. GitHub Actions is now building the release:
echo https://github.com/zyxzjyzjj/cpa-codearts-plugin/actions
echo.
echo After it succeeds:
echo https://github.com/zyxzjyzjj/cpa-codearts-plugin/releases/tag/v%VERSION%
echo https://raw.githubusercontent.com/zyxzjyzjj/cpa-codearts-plugin/master/registry.json
goto :success

:tag_push_failed
echo.
echo ERROR: master was pushed and the local tag exists, but pushing the tag failed.
echo Fix the network or credentials, then run:
echo   git push origin v%VERSION%
goto :abort_release

:git_failed
echo.
echo ERROR: A Git command failed. Review the output above.
goto :abort_release

:abort_release
echo.
echo Release was not completed.
pause
exit /b 1

:success
echo.
pause
exit /b 0
