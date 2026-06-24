"""Simulated cluster environment — provides mock data for demo without real infra."""

from __future__ import annotations

from .scenarios import ALL_SCENARIOS


class SimulatedCluster:
    """A mock cluster that serves pre-generated scenario data.

    In production, this would be replaced by calls to real infrastructure.
    For the demo, it returns realistic-looking logs, configs, and metrics.
    """

    def __init__(self, scenario_name: str = "flink_oom"):
        scenario_fn = ALL_SCENARIOS.get(scenario_name)
        if not scenario_fn:
            msg = f"Unknown scenario '{scenario_name}'. Available: {list(ALL_SCENARIOS.keys())}"
            raise ValueError(msg)
        data = scenario_fn()
        self.context = data["context"]
        self.logs = data.get("logs", {})
        self.config = data.get("config", {})
        self.metrics = data.get("metrics", {})
        self.expected_root_cause = data.get("expected_root_cause", "")
        self.scenario_name = scenario_name

    def get_node_log(self, log_path: str, keywords: str = "", max_lines: int = 50) -> str:
        """Return matching lines from a simulated log file."""
        content = self.logs.get(log_path, "")
        if not content:
            return f"[Simulated] Log file '{log_path}' not found on this cluster."

        lines = content.strip().split("\n")
        if keywords:
            kw = keywords.lower()
            lines = [l for l in lines if kw in l.lower()]

        matched = lines[-max_lines:] if len(lines) > max_lines else lines
        return "\n".join(matched) if matched else f"[Simulated] No lines matching '{keywords}' in {log_path}"

    def get_config(self, config_path: str) -> str:
        """Return a config file content as YAML-like text."""
        import yaml
        data = self.config.get(config_path, {})
        if not data:
            return f"[Simulated] Config file '{config_path}' not found."
        return yaml.dump(data, default_flow_style=False)

    def get_metrics(self, metric_names: list[str] | None = None) -> str:
        """Return metrics as key=value pairs."""
        lines = []
        for k, v in self.metrics.items():
            if metric_names and k not in metric_names:
                continue
            lines.append(f"{k}={v}")
        return "\n".join(lines) if lines else "[Simulated] No metrics available."

    def summary(self) -> str:
        """Human-readable cluster summary."""
        c = self.context
        return (
            f"Cluster: {c.get('cluster_id', 'N/A')}\n"
            f"Region: {c.get('region', 'N/A')}\n"
            f"Component: {c.get('component', 'N/A')} v{c.get('version', 'N/A')}\n"
            f"Problem: {c.get('problem_desc', c.get('problem', 'N/A'))}\n"
            f"Detail: {c.get('detail', '')}"
        )
