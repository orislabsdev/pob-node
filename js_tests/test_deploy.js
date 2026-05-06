const { Web3 } = require('web3');
const web3 = new Web3("http://localhost:8545");

async function run() {
    try {
        const accounts = await web3.eth.getAccounts();
        console.log("Accounts:", accounts);
        if (accounts.length === 0) return;

        const from = accounts[0];
        
        const data = "0x6080604052348015600f57600080fd5b50603f80601d6000396000f3fe6080604052600080fdfea2646970667358221220a23348630018d4df71e3cefc07a976c703b0d771e89ce2189d2b2c93d9370b0264736f6c63430008070033";
        
        const gas = await web3.eth.estimateGas({ from, data });
        console.log("Estimated gas:", gas);
        
        const receipt = await web3.eth.sendTransaction({ from, data, gas });
        console.log("Receipt:", receipt.transactionHash, receipt.contractAddress);
    } catch (e) {
        console.error("Error:", e);
    }
}
run();
