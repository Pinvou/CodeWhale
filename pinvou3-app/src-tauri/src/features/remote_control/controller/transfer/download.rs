//! B→A artifact download extension slot (implemented by DEV-18).
//!
//! The source resolves an authoritative artifact ID to bytes. The shared core
//! enforces ordering, budgets and digests; DEV-18 only implements the source
//! port and never accepts a host path from A.

use super::limits::MAX_TRANSFER_CHUNK_BYTES;

pub(crate) trait DownloadSource: Send + Sync {
    fn total_bytes(&self) -> u64;

    /// Reads one chunk. `Ok(None)` means the source is shorter than advertised,
    /// which the caller must treat as a failed download.
    fn read_chunk(
        &self,
        chunk_index: u64,
        max_bytes: usize,
    ) -> std::result::Result<Option<Vec<u8>>, String>;
}

/// Number of chunks for a bounded payload.
pub(crate) fn chunk_count(total_bytes: u64, chunk_bytes: usize) -> u64 {
    let chunk_bytes = chunk_bytes.min(MAX_TRANSFER_CHUNK_BYTES).max(1) as u64;
    total_bytes.div_ceil(chunk_bytes)
}

/// Byte offset of one chunk index.
pub(crate) fn chunk_offset(chunk_index: u64, chunk_bytes: usize) -> u64 {
    let chunk_bytes = chunk_bytes.min(MAX_TRANSFER_CHUNK_BYTES).max(1) as u64;
    chunk_index.saturating_mul(chunk_bytes)
}

#[cfg(test)]
mod tests {
    use super::*;

    struct MemorySource {
        bytes: Vec<u8>,
    }

    impl DownloadSource for MemorySource {
        fn total_bytes(&self) -> u64 {
            self.bytes.len() as u64
        }

        fn read_chunk(
            &self,
            chunk_index: u64,
            max_bytes: usize,
        ) -> std::result::Result<Option<Vec<u8>>, String> {
            let start = chunk_offset(chunk_index, max_bytes);
            if start >= self.bytes.len() as u64 {
                return Ok(None);
            }
            let end = (start as usize + max_bytes).min(self.bytes.len());
            Ok(Some(self.bytes[start as usize..end].to_vec()))
        }
    }

    #[test]
    fn chunk_arithmetic_is_bounded_and_never_widens_the_protocol_limit() {
        assert_eq!(chunk_count(0, 1024), 0);
        assert_eq!(chunk_count(1024, 1024), 1);
        assert_eq!(chunk_count(1025, 1024), 2);
        // A caller asking for a wider chunk than the protocol allows is clamped.
        assert_eq!(chunk_count(MAX_TRANSFER_CHUNK_BYTES as u64 * 2, usize::MAX), 2);
        assert_eq!(chunk_offset(3, usize::MAX), MAX_TRANSFER_CHUNK_BYTES as u64 * 3);
    }

    #[test]
    fn source_walks_the_payload_in_order_and_reports_the_end() {
        let source = MemorySource {
            bytes: (0..2500u32).map(|value| value as u8).collect(),
        };
        let chunk_bytes = 1024;
        let mut collected = Vec::new();
        for index in 0..chunk_count(source.total_bytes(), chunk_bytes) {
            let chunk = source.read_chunk(index, chunk_bytes).unwrap().unwrap();
            assert!(chunk.len() <= chunk_bytes);
            collected.extend_from_slice(&chunk);
        }
        assert_eq!(collected, source.bytes);
        assert!(source.read_chunk(3, chunk_bytes).unwrap().is_none());
    }
}
