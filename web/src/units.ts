import { useQuery } from "@tanstack/react-query";
import { useMemo } from "react";
import { api, type Asset } from "./api";
import { formatAmount, formatUnits, shortHex } from "./format";

const native: Asset = { token: "0x0000000000000000000000000000000000000000", symbol: "ETH", decimals: 18 };

export interface Units {
  symbol: (asset: string) => string;
  // amount shows base units in token units when the bot reported the token's
  // decimals, else the exact base units.
  amount: (raw: string, asset: string) => string;
  value: (baseUnits: number, asset: string) => string;
}

export function useUnits(instance?: string): Units {
  const assets = useQuery({ queryKey: ["assets", instance], queryFn: () => api.assets(instance!), enabled: Boolean(instance), staleTime: 60_000 });
  return useMemo(() => {
    const byToken = new Map([native, ...(assets.data ?? [])].map((asset) => [asset.token.toLowerCase(), asset]));
    const unit = (asset: string) => byToken.get(asset.toLowerCase());
    const symbol = (asset: string) => unit(asset)?.symbol || shortHex(asset);
    const amount = (raw: string, asset: string) => {
      if (!raw) return "—";
      const known = unit(asset);
      return known ? `${formatUnits(raw, known.decimals)} ${symbol(asset)}` : formatAmount(raw);
    };
    const value = (baseUnits: number, asset: string) => (Number.isFinite(baseUnits) ? amount(BigInt(Math.round(baseUnits)).toString(), asset) : "—");
    return { symbol, amount, value };
  }, [assets.data]);
}
