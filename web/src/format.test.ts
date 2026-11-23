import { describe, expect, it } from "vitest";
import { formatAmount, formatBytes, formatCompact, formatDuration, formatEth, formatGwei, formatRate, formatSeconds, formatUnits, shortHex } from "./format";

describe("format", () => {
  it.each([
    [999, "999 ns"],
    [1_500, "1.5 µs"],
    [250_000, "250 µs"],
    [1_500_000, "1.5 ms"],
    [120_000_000, "120 ms"],
    [2_500_000_000, "2.50 s"],
  ])("duration %d", (input, expected) => {
    expect(formatDuration(input)).toBe(expected);
  });

  it.each([
    [0, "0"],
    [0.25, "0.25"],
    [12.34, "12.3"],
    [12_500, "12.5K"],
    [undefined, "—"],
  ])("rate %s", (input, expected) => {
    expect(formatRate(input)).toBe(expected);
  });

  it("keeps base-unit amounts exact past float precision", () => {
    expect(formatAmount("1267650600228229401496703205376")).toBe("1,267,650,600,228,229,401,496,703,205,376");
    expect(formatAmount("-1000")).toBe("−1,000");
    expect(formatAmount("12")).toBe("12");
  });

  it("scales compact counts and bytes", () => {
    expect(formatCompact(2_500_000)).toBe("2.5M");
    expect(formatBytes(1536)).toBe("1.5 KB");
    expect(formatBytes(5 * 1024 ** 3)).toBe("5.0 GB");
  });

  it.each([
    ["18985454743139370", 18, "0.01899"],
    ["6200000000000000", 18, "0.0062"],
    ["-1903800000000000000", 18, "−1.9038"],
    ["1234567890000000000000", 18, "1,234.6"],
    ["1234567890000000000000000", 18, "1,234,568"],
    ["12345678", 6, "12.346"],
    ["1000000", 6, "1"],
    ["0", 18, "0"],
    ["-1", 18, "−0.000000000000000001"],
    ["5", 0, "5"],
    ["abc", 18, "—"],
  ])("units %s with %d decimals", (raw, decimals, expected) => {
    expect(formatUnits(raw, decimals)).toBe(expected);
  });

  it("shows native amounts in ETH or gwei", () => {
    expect(formatEth("3100000000000000")).toBe("0.0031 ETH");
    expect(formatEth("140000000000")).toBe("140 gwei");
    expect(formatEth(5e9)).toBe("5 gwei");
    expect(formatEth(-2000)).toBe("−0.000002 gwei");
    expect(formatEth("0")).toBe("0 ETH");
    expect(formatEth(undefined)).toBe("—");
    expect(formatGwei("120000")).toBe("0.00012 gwei");
  });

  it("formats uptime and shortens hex", () => {
    expect(formatSeconds(45)).toBe("45s");
    expect(formatSeconds(3700)).toBe("1h 1m");
    expect(formatSeconds(90000)).toBe("1d 1h");
    expect(shortHex("0x1234567890abcdef1234567890abcdef12345678")).toBe("0x123456…345678");
    expect(shortHex("0x1234")).toBe("0x1234");
  });
});
