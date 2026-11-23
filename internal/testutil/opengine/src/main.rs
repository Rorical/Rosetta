//! Offline test bridge to the native OP execution engine. One JSON request per line.
use op_revm::revm::{
    Context, Database, DatabaseCommit, ExecuteEvm,
    context::{BlockEnv, CfgEnv, TxEnv},
    database::InMemoryDB,
    primitives::{Address, Bytes, TxKind, U256},
    state::Bytecode,
};
use op_revm::{DefaultOp, L1BlockInfo, OpBuilder, OpSpecId, OpTransaction};
use serde_json::{Value, json};
use std::{
    error::Error,
    io::{self, BufRead},
    str::FromStr,
};

type Failure = Box<dyn Error>;
fn address(value: &Value) -> Result<Address, Failure> {
    Ok(Address::from_str(value.as_str().ok_or("address missing")?)?)
}
fn uint(value: &Value) -> Result<U256, Failure> {
    if value.is_null() {
        return Ok(U256::ZERO);
    }
    Ok(U256::from_str(
        value.as_str().ok_or("hex integer missing")?,
    )?)
}
fn bytes(value: &Value) -> Result<Bytes, Failure> {
    Ok(Bytes::from_str(value.as_str().unwrap_or("0x"))?)
}
fn account(db: &mut InMemoryDB, addr: Address) -> Result<Value, Failure> {
    let info = db.basic(addr)?.unwrap_or_default();
    let code = db.code_by_hash(info.code_hash)?;
    Ok(
        json!({"balance":format!("{:#x}",info.balance),"nonce":info.nonce,"code":format!("{:#x}",code.original_bytes())}),
    )
}
fn overrides(db: &mut InMemoryDB, values: &Value) -> Result<(), Failure> {
    let Some(values) = values.as_object() else {
        return Ok(());
    };
    for (text, value) in values {
        let addr = Address::from_str(text)?;
        let mut info = db.basic(addr)?.unwrap_or_default();
        if !value["balance"].is_null() {
            info.balance = uint(&value["balance"])?;
        }
        if !value["nonce"].is_null() {
            info.nonce = if let Some(n) = value["nonce"].as_u64() {
                n
            } else {
                uint(&value["nonce"])?.to::<u64>()
            };
        }
        if !value["code"].is_null() {
            info.code = Some(Bytecode::new_raw(bytes(&value["code"])?));
        }
        db.insert_account_info(addr, info);
        if let Some(storage) = value["stateDiff"].as_object() {
            for (key, value) in storage {
                db.insert_account_storage(addr, U256::from_str(key)?, uint(value)?)?;
            }
        }
        if let Some(storage) = value["state"].as_object() {
            db.replace_account_storage(
                addr,
                storage
                    .iter()
                    .map(|(k, v)| Ok((U256::from_str(k)?, uint(v)?)))
                    .collect::<Result<_, Failure>>()?,
            )?;
        }
    }
    Ok(())
}
fn execute(db: &mut InMemoryDB, input: &Value) -> Result<Value, Failure> {
    let mut working = db.clone();
    overrides(&mut working, &input["overrides"])?;
    let before = working.clone();
    let call = &input["call"];
    let from = address(&call["from"])?;
    let nonce = if call["nonce"].is_null() {
        working.basic(from)?.unwrap_or_default().nonce
    } else {
        uint(&call["nonce"])?.to::<u64>()
    };
    let chain = input["chain_id"].as_u64().unwrap_or(8453);
    let number = input["number"].as_u64().unwrap_or(10);
    let timestamp = input["timestamp"].as_u64().unwrap_or(1000);
    let basefee = input["base_fee"].as_u64().unwrap_or(1);
    let fork = if input["fork"] == "isthmus" {
        OpSpecId::ISTHMUS
    } else {
        OpSpecId::JOVIAN
    };
    let mut cfg = CfgEnv::new_with_spec(fork);
    cfg.chain_id = chain;
    cfg.disable_fee_charge = !input["native_fees"].as_bool().unwrap_or(false);
    let block = BlockEnv {
        number: U256::from(number),
        timestamp: U256::from(timestamp),
        basefee,
        gas_limit: input["gas_limit"].as_u64().unwrap_or(32_000_000),
        ..Default::default()
    };
    let l1 = L1BlockInfo {
        l2_block: Some(U256::from(number)),
        l1_base_fee: U256::from(1_000_000_000u64),
        l1_base_fee_scalar: U256::from(1000),
        l1_blob_base_fee: Some(U256::from(1)),
        l1_blob_base_fee_scalar: Some(U256::from(1000)),
        operator_fee_scalar: Some(U256::from(2)),
        operator_fee_constant: Some(U256::from(50)),
        da_footprint_gas_scalar: Some(400),
        ..Default::default()
    };
    let mut builder = OpTransaction::builder()
        .base(
            TxEnv::builder()
                .caller(from)
                .nonce(nonce)
                .gas_limit(uint(&call["gas"])?.to::<u64>())
                .kind(if call["to"].is_null() {
                    TxKind::Create
                } else {
                    TxKind::Call(address(&call["to"])?)
                })
                .value(uint(&call["value"])?)
                .data(bytes(&call["data"])?)
                .chain_id(Some(chain))
                .gas_price(uint(&call["maxFeePerGas"])?.to::<u128>())
                .gas_priority_fee(Some(uint(&call["maxPriorityFeePerGas"])?.to::<u128>())),
        )
        .enveloped_tx(Some(bytes(&input["envelope"])?));
    if call["type"] == "0x7e" {
        builder = builder
            .is_deposit_tx()
            .source_hash(op_revm::revm::primitives::B256::from_str(
                call["sourceHash"]
                    .as_str()
                    .ok_or("deposit source hash missing")?,
            )?)
            .mint(uint(&call["mint"])?.to::<u128>());
    }
    let tx = builder
        .build()
        .map_err(|err| format!("transaction build: {err:?}"))?;
    let mut evm = Context::op()
        .with_db(working)
        .with_cfg(cfg)
        .with_block(block)
        .with_chain(l1)
        .build_op();
    let result = evm.transact(tx)?;
    let output = result.result.output().cloned().unwrap_or_default();
    let created = match &result.result {
        op_revm::revm::context::result::ExecutionResult::Success { output, .. } => {
            output.address().copied()
        }
        _ => None,
    };
    let mut pre = serde_json::Map::new();
    let mut post = serde_json::Map::new();
    let mut original = before;
    for (addr, changed) in &result.state {
        if !changed.is_touched() {
            continue;
        }
        let old = original.basic(*addr)?.unwrap_or_default();
        let mut p = account(&mut original, *addr)?;
        let mut q =
            json!({"balance":format!("{:#x}",changed.info.balance),"nonce":changed.info.nonce});
        if changed.info.code_hash != old.code_hash {
            if let Some(code) = &changed.info.code {
                q["code"] = json!(format!("{:#x}", code.original_bytes()));
            }
        }
        if changed.info.balance == old.balance {
            p.as_object_mut()
                .ok_or("account missing")?
                .remove("balance");
            q.as_object_mut()
                .ok_or("account missing")?
                .remove("balance");
        }
        let mut old_slots = serde_json::Map::new();
        let mut new_slots = serde_json::Map::new();
        for (key, slot) in &changed.storage {
            if slot.is_changed() {
                old_slots.insert(
                    format!("{:#066x}", key),
                    json!(format!("{:#066x}", slot.original_value)),
                );
                new_slots.insert(
                    format!("{:#066x}", key),
                    json!(format!("{:#066x}", slot.present_value)),
                );
            }
        }
        p["storage"] = Value::Object(old_slots);
        q["storage"] = Value::Object(new_slots);
        pre.insert(format!("{:#x}", addr), p);
        post.insert(format!("{:#x}", addr), q);
    }
    if input["commit"].as_bool().unwrap_or(false) {
        db.commit(result.state);
    }
    let logs: Vec<Value> = result.result.logs().iter().map(|log| json!({"address":format!("{:#x}",log.address),"topics":log.data.topics().iter().map(|t|format!("{t:#x}")).collect::<Vec<_>>(),"data":format!("{:#x}",log.data.data),"position":0})).collect();
    Ok(
        json!({"logs":logs,"output":format!("{:#x}",output),"gasUsed":format!("0x{:x}",result.result.tx_gas_used()),"success":result.result.is_success(),"created":created.map(|addr|format!("{addr:#x}")),"pre":pre,"post":post}),
    )
}
fn handle(
    db: &mut InMemoryDB,
    checkpoints: &mut Vec<InMemoryDB>,
    value: &Value,
) -> Result<Value, Failure> {
    match value["method"].as_str().ok_or("method missing")? {
        "checkpoint" => {
            checkpoints.push(db.clone());
            Ok(Value::Null)
        }
        "restore" => {
            *db = checkpoints.pop().ok_or("checkpoint missing")?;
            Ok(Value::Null)
        }
        "set" => {
            overrides(db, &value["accounts"])?;
            Ok(Value::Null)
        }
        "account" => account(db, address(&value["address"])?),
        "execute" => execute(db, value),
        _ => Err("unknown test-engine method".into()),
    }
}
fn main() -> Result<(), Failure> {
    let mut db = InMemoryDB::default();
    let mut checkpoints = Vec::new();
    for line in io::stdin().lock().lines() {
        let input: Value = serde_json::from_str(&line?)?;
        let output = match handle(&mut db, &mut checkpoints, &input) {
            Ok(value) => json!({"result":value}),
            Err(error) => json!({"error":error.to_string()}),
        };
        println!("{}", output);
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use op_revm::estimate_tx_compressed_size;

    #[test]
    fn l1_reserve_covers_native_compression_for_signed_inputs() {
        let mut seed = 0x7a613b21u32;
        for unsigned in [1usize, 68, 100, 255, 256, 1024, 4096, 65535, 1 << 20] {
            let size_argument = 2 * (unsigned + 68) + 16;
            let tx_size = size_argument + 68;
            let flz_bound = tx_size + tx_size / 255 + 16;
            let estimate_bound = (flz_bound as u64 * 836_500)
                .saturating_sub(42_585_600)
                .max(100_000_000);
            for repeated in [false, true] {
                let data: Vec<u8> = (0..unsigned + 68)
                    .map(|_| {
                        seed ^= seed << 13;
                        seed ^= seed >> 17;
                        seed ^= seed << 5;
                        if repeated { 0 } else { seed as u8 }
                    })
                    .collect();
                assert!(estimate_tx_compressed_size(&data) <= estimate_bound);
            }
        }
    }
}
