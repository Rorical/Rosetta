#!/usr/bin/env python3
"""Prepare unsigned executor deployment/admin calls without RPC or signing."""
import argparse
import json
import re
import subprocess
from pathlib import Path


def cast(*args):
    return subprocess.check_output(["cast", *map(str, args)], text=True).strip()


def address(value):
    if not isinstance(value, str) or not re.fullmatch(r"0x[0-9a-fA-F]{40}", value):
        raise ValueError("invalid setup address")
    return value


def prepare(artifact, setup):
    owner, operator = address(setup["owner"]), address(setup["operator"])
    # executeHot senders are immutable; unused places hold the zero address.
    hot = [address(sender) for sender in setup.get("hot_senders", [])]
    if len(hot) > 2:
        raise ValueError("at most two hot senders")
    hot += ["0x" + "00" * 20] * (2 - len(hot))
    chain, nonce = int(setup["chain_id"]), int(setup["owner_nonce"])
    if chain <= 0 or nonce < 0 or nonce >= 2**64 - 1000:
        raise ValueError("invalid chain or owner nonce")
    creation = artifact["bytecode"]["object"]
    runtime = artifact["deployedBytecode"]["object"]
    if len(bytes.fromhex(runtime.removeprefix("0x"))) > 24576:
        raise ValueError("executor runtime exceeds EIP-170")
    if not re.fullmatch(r"(?:0x)?[0-9a-fA-F]+", creation):
        raise ValueError("unlinked or malformed creation code")
    computed = cast("compute-address", owner, "--nonce", nonce)
    matches = re.findall(r"0x[0-9a-fA-F]{40}", computed)
    if len(matches) != 1:
        raise ValueError("unexpected compute-address output")
    executor = matches[0]
    transactions = []

    def add(purpose, data, to=None):
        tx = {"purpose": purpose, "from": owner, "chainId": hex(chain),
              "nonce": hex(nonce + len(transactions)), "value": "0x0", "data": data}
        if to:
            tx["to"] = to
        transactions.append(tx)

    constructor = cast("abi-encode", "constructor(address,address[2])", owner, "[" + ",".join(hot) + "]")
    add("deploy_paused_executor", "0x" + creation.removeprefix("0x") + constructor[2:])
    # The constructor already enables the owner as an operator. The executor
    # keeps no pool or token registry: routes carry their pool parameters.
    if operator.lower() != owner.lower():
        add("enable_operator", cast("calldata", "setOperator(address,bool)", operator, "true"), executor)
    # Activation is separated so owner review and code verification can occur
    # while every trading call still reverts with Unavailable.
    activation = {"from": owner, "to": executor, "chainId": hex(chain),
                  "nonce": hex(nonce + len(transactions)), "value": "0x0",
                  "data": cast("calldata", "setPaused(bool)", "false")}
    return {"executor": executor, "unsigned_only": True, "transactions": transactions,
            "activation_after_verification": activation,
            "required_checks": ["fresh owner nonce", "deployment gas and total setup cost",
                                "deployed runtime code hash and immutable owner/chain",
                                "operator funding and bounded allowances"]}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--artifact", required=True)
    parser.add_argument("--setup", required=True)
    parser.add_argument("--out", required=True)
    args = parser.parse_args()
    result = prepare(json.loads(Path(args.artifact).read_text()), json.loads(Path(args.setup).read_text()))
    # Never overwrite a previously reviewed bundle.
    with Path(args.out).open("x") as output:
        json.dump(result, output, indent=2)
        output.write("\n")


if __name__ == "__main__":
    main()
