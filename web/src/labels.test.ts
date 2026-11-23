import { describe, expect, it } from "vitest";
import { skipLabel } from "./labels";

describe("skip reasons", () => {
  it.each([
    ["below_build_rank", "Below build rank"],
    ["context_older_than_anchor blocks=21-30 age=<2s", "Context older than anchor blocks=21-30 age=<2s"],
    ["build: reactive economics unavailable", "build: reactive economics unavailable"],
  ])("%s", (reason, expected) => {
    expect(skipLabel(reason)).toBe(expected);
  });
});
