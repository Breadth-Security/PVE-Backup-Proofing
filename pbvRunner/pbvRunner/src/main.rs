//use std::fmt::Debug;

//Adding Tracing / systemd logging layers
use tracing::{info, warn};
use tracing_subscriber::prelude::*;
use tracing_systemd::SystemdLayer;
//tracing 

use std::time::Duration;
use tokio::signal;
use tokio::time::sleep;

//Main entry point for daemon to be managed (and scheduled) by systemd
#[tokio::main]
async fn main() {
    tracing_subscriber::registry()
        .with(SystemdLayer::stdout())
        .init();

    info!("PBV Daemon started successfully");
    //println!("Daemon started successfully");
    
    // Create a cancellation token or track select loop
    tokio::select! {
        _ = async {
            loop {
                // Check for any pbv related actions to perform 
                // todo: Add scheduling based on config file
                info!("Executing pbv check for newly unverified backups");
                sleep(Duration::from_secs(10)).await;
            }
        } => {},
        
        // Intercept standard termination signals for graceful shutdown
        _ = signal::ctrl_c() => {
            info!("Termination signal received. PBV Daemon shutting down cleanly...");
        }
    }
}