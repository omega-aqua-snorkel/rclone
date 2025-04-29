#!/bin/bash

# Ensure the input environment variable is set
if [ -z "$RCLONE_COMMANDS" ]; then
    echo "Error: RCLONE_COMMANDS environment variable is not set."
    exit 1
fi

# If RCLONE_CONFIG_DATA is set, store it in the rclone config file
if [ ! -z "$RCLONE_CONFIG_DATA" ]; then
    echo "Storing rclone config..."
    mkdir -p ~/.config/rclone/
    echo "$RCLONE_CONFIG_DATA" > ~/.config/rclone/rclone.conf
    if [ $? -eq 0 ]; then
        echo "rclone config stored successfully."
    else
        echo "Error: Failed to store rclone config."
        exit 1
    fi
fi

# Loop through each line of the input variable and execute the commands
echo "$RCLONE_COMMANDS" | while read -r line; do
    # Skip empty lines
    if [ -z "$line" ]; then
        continue
    fi
    
    # Execute the rclone command
    echo "Executing: $line"
    $line
    if [ $? -eq 0 ]; then
        echo "Success: $line"
    else
        echo "Error: Failed to execute $line"
    fi
done
