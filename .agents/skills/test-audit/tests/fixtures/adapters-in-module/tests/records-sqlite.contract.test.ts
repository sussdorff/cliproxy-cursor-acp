import { runRecordsContract } from "../src/ports/records/contract";
import { SqliteRecords } from "../src/records/sqlite";

runRecordsContract(() => new SqliteRecords());
