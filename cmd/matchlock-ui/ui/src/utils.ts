export function shortDigest(value: string): string {
  if (!value) {
    return "-";
  }
  if (value.length <= 16) {
    return value;
  }
  return `${value.slice(0, 16)}...`;
}

export function statusTone(status: string): string {
  if (status === "running") {
    return "running";
  }
  if (status === "crashed") {
    return "crashed";
  }
  return "stopped";
}

export function formatCreatedAt(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) {
    return "-";
  }
  return date.toLocaleString();
}
