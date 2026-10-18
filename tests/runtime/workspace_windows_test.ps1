# Run with Windows PowerShell 5.1, not pwsh. The native docker.exe stub emits
# real stderr/exit codes, exercising legacy NativeCommandError behavior.
$ErrorActionPreference = 'Stop'
if ($PSVersionTable.PSVersion.Major -ne 5 -or $PSVersionTable.PSVersion.Minor -ne 1) {
    throw 'This regression requires the actual Windows PowerShell 5.1 executable.'
}

$repoRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$bootstrap = Join-Path $repoRoot 'runtime/up.ps1'
$powershellExe = (Get-Command powershell.exe -CommandType Application).Source
$tempRoot = [System.IO.Path]::GetFullPath([System.IO.Path]::GetTempPath())
$testRoot = Join-Path $tempRoot ('ckad-win-bootstrap-' + [Guid]::NewGuid().ToString('N'))
$previousPath = $env:PATH
$previousScenario = $env:CKAD_BOOTSTRAP_TEST_SCENARIO
$previousCalls = $env:CKAD_BOOTSTRAP_TEST_CALLS
New-Item -ItemType Directory -Path $testRoot | Out-Null
try {
    $source = @'
using System;
using System.IO;
public static class CkadDockerBootstrapStub {
    public static int Main(string[] args) {
        File.AppendAllText(Environment.GetEnvironmentVariable("CKAD_BOOTSTRAP_TEST_CALLS"), String.Join(" ", args) + "\n");
        string scenario = Environment.GetEnvironmentVariable("CKAD_BOOTSTRAP_TEST_SCENARIO");
        if (args.Length > 1 && args[0] == "network" && args[1] == "inspect") {
            if (scenario == "inspect-failure") {
                Console.Error.WriteLine("Error response from daemon: connection refused"); return 2;
            }
            if (scenario == "inspect-empty-failure") return 2;
            if (scenario == "inspect-mixed-failure") {
                Console.Error.WriteLine("Error response from daemon: network kind not found");
                Console.Error.WriteLine("Error response from daemon: connection refused"); return 2;
            }
            if (scenario == "missing-two-lines") {
                Console.Error.WriteLine("Error response from daemon: network kind not found");
                Console.Error.WriteLine("Error: No such network: kind"); return 1;
            }
            if (scenario == "missing" || scenario.StartsWith("create-")) {
                Console.Error.WriteLine("Error response from daemon: network kind not found"); return 1;
            }
            Console.WriteLine(scenario == "wrong-driver" ? "overlay" : "bridge"); return 0;
        }
        if (args.Length > 1 && args[0] == "network" && args[1] == "create") {
            if (scenario == "create-failure") { Console.Error.WriteLine("simulated creation failure"); return 17; }
            if (scenario == "create-exit-failure") return 18;
            Console.WriteLine("test-only-network-id"); return 0;
        }
        if (args.Length > 0 && args[0] == "compose") {
            if (scenario == "compose-failure") { Console.Error.WriteLine("simulated Compose failure"); return 19; }
            if (scenario == "compose-exit-failure") return 20;
            Console.WriteLine("test-only workspace started"); return 0;
        }
        Console.Error.WriteLine("unexpected test command"); return 99;
    }
}
'@
    Add-Type -TypeDefinition $source -Language CSharp -OutputAssembly (Join-Path $testRoot 'docker.exe') -OutputType ConsoleApplication
    $env:PATH = $testRoot + [System.IO.Path]::PathSeparator + $previousPath
    $inspect = 'network inspect --format {{.Driver}} kind'
    $create = 'network create --driver bridge --label io.x-k8s.kind.role=network kind'
    $compose = 'compose up -d --build workspace'
    $cases = @(
        @{ Name = 'missing'; Success = $true; Calls = @($inspect, $create, $compose) },
        @{ Name = 'missing-two-lines'; Success = $true; Calls = @($inspect, $create, $compose) },
        @{ Name = 'existing'; Success = $true; Calls = @($inspect, $compose) },
        @{ Name = 'wrong-driver'; Success = $false; Calls = @($inspect) },
        @{ Name = 'inspect-failure'; Success = $false; Calls = @($inspect) },
        @{ Name = 'inspect-empty-failure'; Success = $false; Calls = @($inspect) },
        @{ Name = 'inspect-mixed-failure'; Success = $false; Calls = @($inspect) },
        @{ Name = 'create-failure'; Success = $false; Calls = @($inspect, $create) },
        @{ Name = 'create-exit-failure'; Success = $false; Calls = @($inspect, $create) },
        @{ Name = 'compose-failure'; Success = $false; Calls = @($inspect, $compose) },
        @{ Name = 'compose-exit-failure'; Success = $false; Calls = @($inspect, $compose) }
    )
    $failures = @()
    Write-Output ('Windows PowerShell ' + $PSVersionTable.PSVersion)
    foreach ($case in $cases) {
        $env:CKAD_BOOTSTRAP_TEST_SCENARIO = $case.Name
        $env:CKAD_BOOTSTRAP_TEST_CALLS = Join-Path $testRoot ($case.Name + '.calls')
        $startInfo = New-Object System.Diagnostics.ProcessStartInfo
        $startInfo.FileName = $powershellExe
        $startInfo.Arguments = '-NoProfile -ExecutionPolicy Bypass -File "' + $bootstrap + '"'
        $startInfo.UseShellExecute = $false
        $startInfo.CreateNoWindow = $true
        $startInfo.RedirectStandardOutput = $true
        $startInfo.RedirectStandardError = $true
        $process = New-Object System.Diagnostics.Process
        $process.StartInfo = $startInfo
        [void]$process.Start()
        $stdoutTask = $process.StandardOutput.ReadToEndAsync()
        $stderrTask = $process.StandardError.ReadToEndAsync()
        if (-not $process.WaitForExit(30000)) { $process.Kill(); throw ('Timed out: ' + $case.Name) }
        $stdout = $stdoutTask.Result
        $stderr = $stderrTask.Result
        $code = $process.ExitCode
        $process.Dispose()
        $calls = @(Get-Content -LiteralPath $env:CKAD_BOOTSTRAP_TEST_CALLS)
        $correctExit = (($code -eq 0) -eq $case.Success)
        $correctCalls = (($calls -join "`n") -ceq ($case.Calls -join "`n"))
        if (-not $correctExit -or -not $correctCalls) {
            $failures += $case.Name
            Write-Output ('FAIL ' + $case.Name + ' exit=' + $code + ' calls=' + ($calls -join ' | '))
            Write-Output ($stdout + $stderr)
        } else {
            Write-Output ('PASS ' + $case.Name + ' exit=' + $code)
        }
    }
    if ($failures.Count -gt 0) { throw ('Failed cases: ' + ($failures -join ', ')) }
    Write-Output ('ALL WINDOWS BOOTSTRAP CHECKS PASSED (' + $cases.Count + ' scenarios; native docker.exe stub; no real Docker mutations)')
} finally {
    $env:PATH = $previousPath
    $env:CKAD_BOOTSTRAP_TEST_SCENARIO = $previousScenario
    $env:CKAD_BOOTSTRAP_TEST_CALLS = $previousCalls
    # Delete only this exact test-created directory below the system temp root.
    $resolvedTestRoot = [System.IO.Path]::GetFullPath($testRoot)
    $tempPrefix = $tempRoot.TrimEnd([System.IO.Path]::DirectorySeparatorChar) + [System.IO.Path]::DirectorySeparatorChar
    if (-not $resolvedTestRoot.StartsWith($tempPrefix, [System.StringComparison]::OrdinalIgnoreCase) -or
        (Split-Path -Leaf $resolvedTestRoot) -notmatch '^ckad-win-bootstrap-[0-9a-f]{32}$') {
        throw ('Refusing cleanup outside the verified test temp directory: ' + $resolvedTestRoot)
    }
    Remove-Item -LiteralPath $resolvedTestRoot -Recurse -Force
}
