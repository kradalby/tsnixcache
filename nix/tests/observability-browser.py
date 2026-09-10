# Copyright (c) 2026 Kristoffer Dalby
# SPDX-License-Identifier: BSD-3-Clause

import json
import math
import pathlib
import re
import time
import urllib.parse
import urllib.request
from typing import NotRequired, TypedDict, cast

from playwright.sync_api import Page, sync_playwright


class Sample(TypedDict):
    value: NotRequired[tuple[float, str]]
    values: NotRequired[list[tuple[float, str]]]


class Target(TypedDict):
    expr: str


class Panel(TypedDict):
    title: str
    type: str
    targets: list[Target]


class Dashboard(TypedDict):
    panels: list[Panel]


class FrameSchema(TypedDict):
    fields: list[dict[str, str]]


class FrameData(TypedDict):
    values: list[list[float | None]]


class Frame(TypedDict):
    schema: FrameSchema
    data: FrameData


class QueryResult(TypedDict):
    error: NotRequired[str]
    frames: NotRequired[list[Frame]]


def scalar(sample: Sample) -> float:
    value = sample.get("value")
    assert value is not None, sample
    return float(value[1])


CONTROL = pathlib.Path("/run/observability")
TITLES = ("Store disk used", "Store paths")


def request(url: str, data: object = None) -> dict[str, object]:
    body = None if data is None else json.dumps(data).encode()
    req = urllib.request.Request(url, data=body, headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=10) as response:
        return cast(dict[str, object], json.load(response))


def query(expr: str, when: float | None = None) -> list[Sample]:
    args: dict[str, str | float] = {"query": expr}
    if when is not None:
        args["time"] = when
    response = request("http://localhost:9090/api/v1/query?" + urllib.parse.urlencode(args))
    return cast(dict[str, list[Sample]], response["data"])["result"]


def grafana_values(target: Target, when: float) -> list[float]:
    model = dict(target, refId="A", datasource={"type": "prometheus", "uid": "prometheus"})
    response = request(
        "http://localhost:3000/api/ds/query",
        {"queries": [model], "from": str(int((when - 3600) * 1000)), "to": str(int(when * 1000))},
    )
    result = cast(dict[str, QueryResult], response["results"])["A"]
    assert not result.get("error"), result
    values: list[float] = []
    for frame in result.get("frames", []):
        for index, field in enumerate(frame["schema"]["fields"]):
            if field["type"] == "number":
                values.extend(x for x in frame["data"]["values"][index] if x is not None)
    return values


def save(name: str, value: object) -> None:
    temporary = CONTROL / (name + ".tmp")
    temporary.write_text(json.dumps(value))
    temporary.replace(CONTROL / name)


def check_phase(page: Page, panels: dict[str, Panel], phase: str) -> dict[str, object]:
    absent = phase in ("failed-scrape", "removed-target")
    panel_evidence: dict[str, object] = {}
    evidence: dict[str, object] = {"phase": phase, "panels": panel_evidence}
    for title in TITLES:
        target = panels[title]["targets"][0]
        values = query(target["expr"])
        rendered_values = grafana_values(target, time.time())
        assert bool(values) != absent, (phase, title, values)
        assert bool(rendered_values) != absent, (phase, title, rendered_values)
        panel = page.get_by_role("region", name=title, exact=True)
        assert panel.count() == 1, (title, panel.count())
        text = panel.inner_text()
        displayed = None
        if absent:
            label = panel.get_by_text("Unavailable", exact=True)
            assert label.count() == 1, (phase, title, text)
            colors = cast(
                list[str],
                label.evaluate("""element => {
              const colors = [];
              const panel = element.closest('section[aria-labelledby]');
              for (let node = element; node; node = node.parentElement) {
                const style = getComputedStyle(node);
                colors.push(style.color, style.backgroundColor, style.backgroundImage);
                if (node === panel) break;
              }
              return colors;
            }"""),
            )
            for color in re.findall(r"rgba?\([^)]+\)", " ".join(colors)):
                components = [float(x) for x in re.findall(r"[\d.]+", color)]
                if len(components) < 3 or (len(components) == 4 and components[3] == 0):
                    continue
                assert max(components[:3]) - min(components[:3]) <= 25, (
                    phase,
                    title,
                    color,
                    colors,
                )
        else:
            assert "Unavailable" not in text, (phase, title, text)
            value = panel.locator('div[style*="font-weight: 500"]')
            assert value.count() == 1, (phase, title, value.count(), text)
            display = value.inner_text().strip()
            match = re.fullmatch(r"(-?[\d,.]+)\s*([kKMGTPE%]?)", display)
            assert match, (phase, title, display)
            scale = {"k": 1e3, "K": 1e3, "M": 1e6, "G": 1e9, "T": 1e12, "P": 1e15, "E": 1e18}.get(
                match[2], 1
            )
            number = match[1].replace(",", "")
            displayed = float(number) * scale
            decimals = len(number.split(".")[1]) if "." in number else 0
            tolerance = 0.5 * (10**-decimals) * scale + 1e-6
            expected = scalar(values[0])
            if title == "Store disk used":
                tolerance = max(tolerance, 0.2)
            assert abs(displayed - expected) <= tolerance, (
                phase,
                title,
                displayed,
                expected,
                tolerance,
            )
            assert all(math.isfinite(scalar(x)) for x in values), values
        panel_evidence[title] = {
            "prometheus": values,
            "grafana": rendered_values,
            "text": text,
            "displayed": displayed,
        }
    historical = query("tsnixcache_store_paths[10m]")
    assert historical and historical[0].get("values", []), historical
    evidence["historical_samples"] = len(historical[0].get("values", []))
    return evidence


def freshness(panels: dict[str, Panel]) -> None:
    latest = scalar(query('timestamp(up{job="tsnixcache"})')[0])
    future = latest + 180
    panel_evidence: dict[str, object] = {}
    evidence: dict[str, object] = {"time": future, "panels": panel_evidence}
    for title in TITLES:
        raw = (
            "tsnixcache_store_paths"
            if title == "Store paths"
            else "tsnixcache_store_disk_used_bytes"
        )
        assert query(raw, future), raw
        assert query('up{job="tsnixcache"}', future)
        target = panels[title]["targets"][0]
        assert not query(target["expr"], future), target
        assert not grafana_values(target, future), target
        panel_evidence[title] = "unavailable while raw gauge and up remain in lookback"
    save("freshness.json", evidence)


def main() -> None:
    save("status.json", {"phase": "starting"})
    dashboard = cast(
        Dashboard, request("http://localhost:3000/api/dashboards/uid/tsnixcache")["dashboard"]
    )
    panels = {p["title"]: p for p in dashboard["panels"] if p.get("type") != "row"}
    save("dashboard.json", dashboard)
    with sync_playwright() as playwright:
        browser = playwright.chromium.launch(args=["--no-sandbox", "--disable-gpu"])
        page = browser.new_page(viewport={"width": 1600, "height": 1200})
        page.goto(
            "http://localhost:3000/d/tsnixcache/tsnixcache?var-datasource=prometheus&from=now-1h&to=now&refresh=5s"
        )
        previous = None
        while True:
            phase = (CONTROL / "phase").read_text().strip()
            if phase == "stop":
                break
            if phase == previous:
                page.wait_for_timeout(200)
                continue
            deadline = time.monotonic() + 90
            while True:
                try:
                    evidence = check_phase(page, panels, phase)
                    break
                except Exception as error:
                    save("attempt.json", {"phase": phase, "error": repr(error)})
                    if time.monotonic() >= deadline:
                        page.screenshot(
                            path=str(CONTROL / (phase + "-failure.png")), full_page=True
                        )
                        save(
                            "failure.json",
                            {
                                "phase": phase,
                                "error": repr(error),
                                "url": page.url,
                                "body": page.locator("body").inner_text(),
                            },
                        )
                        (CONTROL / "failure.html").write_text(page.content())
                        raise
                    page.wait_for_timeout(500)
            if phase == "recovered-target":
                freshness(panels)
            page.screenshot(path=str(CONTROL / (phase + ".png")), full_page=True)
            save(phase + ".json", evidence)
            save("status.json", evidence)
            previous = phase
        browser.close()


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        save("status.json", {"phase": "error", "error": repr(error)})
        raise
