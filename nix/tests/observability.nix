# Copyright (c) 2026 Kristoffer Dalby
# SPDX-License-Identifier: BSD-3-Clause

{
  pkgs,
  tsnixcache,
  tsnixcacheModule,
  grafanaDashboards,
}:
let
  browser = pkgs.writers.writePython3Bin "observability-browser" {
    libraries = [ pkgs.python3Packages.playwright ];
    flakeIgnore = [ "E501" ];
  } (builtins.readFile ./observability-browser.py);
in
{
  name = "tsnixcache-observability";
  nodes.machine = { ... }: {
    imports = [ tsnixcacheModule ];
    virtualisation.memorySize = 4096;
    virtualisation.cores = 2;
    environment.systemPackages = [
      pkgs.nix
      pkgs.jq
      pkgs.curl
    ];
    services.tsnixcache = {
      enable = true;
      package = tsnixcache;
      listen = [ "127.0.0.1:5000" ];
    };
    services.prometheus = {
      enable = true;
      listenAddress = "127.0.0.1";
      scrapeConfigs = [
        {
          job_name = "tsnixcache";
          scrape_interval = "1s";
          scrape_timeout = "1s";
          file_sd_configs = [
            {
              files = [ "/run/tsnixcache-targets.json" ];
              refresh_interval = "1s";
            }
          ];
        }
      ];
    };
    services.grafana = {
      enable = true;
      settings = {
        security.secret_key = "test-only-observability-key";
        server = {
          http_addr = "127.0.0.1";
          http_port = 3000;
        };
        "auth.anonymous" = {
          enabled = true;
          org_role = "Viewer";
        };
        analytics = {
          reporting_enabled = false;
          check_for_updates = false;
          check_for_plugin_updates = false;
        };
      };
      provision = {
        enable = true;
        datasources.settings = {
          apiVersion = 1;
          datasources = [
            {
              name = "Prometheus";
              type = "prometheus";
              uid = "prometheus";
              access = "proxy";
              url = "http://127.0.0.1:9090";
              isDefault = true;
              jsonData.timeInterval = "1s";
            }
          ];
        };
        dashboards.settings = {
          apiVersion = 1;
          providers = [
            {
              name = "tsnixcache";
              options.path = grafanaDashboards;
            }
          ];
        };
      };
    };
    systemd.services.observability-probe = {
      environment.PLAYWRIGHT_BROWSERS_PATH = "${pkgs.playwright-driver.browsers}";
      serviceConfig = {
        Type = "exec";
        RemainAfterExit = true;
        ExecStart = "${browser}/bin/observability-browser";
      };
    };
  };
  testScript = ''
    import json, shlex

    start_all()
    machine.wait_for_unit("tsnixcache.service")
    machine.wait_for_unit("prometheus.service")
    machine.wait_for_unit("grafana.service")
    machine.wait_for_open_port(5000)
    machine.succeed("nix-store --add /etc/hostname > /dev/null")
    machine.wait_until_succeeds("curl -fsS http://localhost:5000/health | jq -e .store_reachable")
    machine.wait_for_open_port(9090)
    machine.wait_for_open_port(3000)
    machine.succeed("install -d -m 755 /run/observability")

    def targets(enabled):
        body = json.dumps([{"targets": ["127.0.0.1:5000"]}] if enabled else [])
        machine.succeed("printf '%s' " + shlex.quote(body) + " > /run/tsnixcache-targets.json.tmp; mv /run/tsnixcache-targets.json.tmp /run/tsnixcache-targets.json")

    def phase(name):
        machine.succeed("printf '%s' " + shlex.quote(name) + " > /run/observability/phase.tmp; mv /run/observability/phase.tmp /run/observability/phase")
        check = '.phase == ' + json.dumps(name) + ' and .error == null'
        try:
            machine.wait_until_succeeds("jq -e " + shlex.quote(check) + " /run/observability/status.json", timeout=180)
        except Exception:
            machine.execute("cat /run/observability/status.json /run/observability/attempt.json; journalctl -u observability-probe --no-pager -n 40")
            machine.copy_from_machine("/run/observability")
            raise

    targets(True)
    machine.wait_until_succeeds("curl -fsS http://localhost:3000/api/dashboards/uid/tsnixcache > /dev/null")
    machine.succeed("printf healthy > /run/observability/phase; systemctl start observability-probe")
    phase("healthy")
    healthy = json.loads(machine.succeed("cat /run/observability/healthy.json"))
    before = float(healthy["panels"]["Store paths"]["prometheus"][0]["value"][1])
    machine.succeed("systemctl stop tsnixcache")
    phase("failed-scrape")
    machine.succeed("mkdir /tmp/observability-paths; for n in $(seq 1 64); do echo recovery-$n > /tmp/observability-paths/$n; done; nix-store --add /tmp/observability-paths/*; systemctl start tsnixcache")
    machine.wait_for_open_port(5000)
    phase("recovered-scrape")
    recovered = json.loads(machine.succeed("cat /run/observability/recovered-scrape.json"))
    after = float(recovered["panels"]["Store paths"]["prometheus"][0]["value"][1])
    assert after >= before + 64, (before, after)
    assert recovered["panels"]["Store paths"]["displayed"] != healthy["panels"]["Store paths"]["displayed"]
    targets(False)
    machine.wait_until_succeeds("curl -fsS 'http://localhost:9090/api/v1/targets?state=active' | jq -e '.data.activeTargets | length == 0'")
    machine.succeed("curl -fsS http://localhost:5000/metrics > /dev/null")
    phase("removed-target")
    targets(True)
    phase("recovered-target")
    machine.succeed("test -s /run/observability/freshness.json; printf stop > /run/observability/phase.tmp; mv /run/observability/phase.tmp /run/observability/phase")
    machine.wait_until_succeeds("test $(systemctl show -p ExecMainExitTimestampMonotonic --value observability-probe) -gt 0")
    assert machine.succeed("systemctl show -p Result --value observability-probe").strip() == "success"
    machine.copy_from_machine("/run/observability")
  '';
}
