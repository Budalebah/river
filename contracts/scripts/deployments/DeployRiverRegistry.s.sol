// SPDX-License-Identifier: MIT
pragma solidity ^0.8.23;

// interfaces
import {IDiamond} from "contracts/src/diamond/IDiamond.sol";

// helpers
import {DiamondDeployer} from "../common/DiamondDeployer.s.sol";
import {Diamond} from "contracts/src/diamond/Diamond.sol";

import {DeployDiamondCut} from "contracts/scripts/deployments/facets/DeployDiamondCut.s.sol";
import {DeployDiamondLoupe} from "contracts/scripts/deployments/facets/DeployDiamondLoupe.s.sol";
import {DeployIntrospection} from "contracts/scripts/deployments/facets/DeployIntrospection.s.sol";
import {DeployOwnable} from "contracts/scripts/deployments/facets/DeployOwnable.s.sol";

import {NodeRegistryHelper} from "contracts/test/river/registry/NodeRegistryHelper.sol";
import {StreamRegistryHelper} from "contracts/test/river/registry/StreamRegistryHelper.sol";
import {OperatorRegistryHelper} from "contracts/test/river/registry/OperatorRegistryHelper.sol";
import {RiverConfigHelper} from "contracts/test/river/registry/RiverConfigHelper.sol";

// deployers
import {DeployMultiInit} from "contracts/scripts/deployments/DeployMultiInit.s.sol";
import {DeployRiverConfig} from "./facets/DeployRiverConfig.s.sol";

// facets
import {NodeRegistry} from "contracts/src/river/registry/facets/node/NodeRegistry.sol";
import {StreamRegistry} from "contracts/src/river/registry/facets/stream/StreamRegistry.sol";
import {OperatorRegistry} from "contracts/src/river/registry/facets/operator/OperatorRegistry.sol";
import {RiverConfig} from "contracts/src/river/registry/facets/config/RiverConfig.sol";

import {MultiInit} from "contracts/src/diamond/initializers/MultiInit.sol";

contract DeployRiverRegistry is DiamondDeployer {
  DeployDiamondCut internal cutHelper = new DeployDiamondCut();
  DeployDiamondLoupe internal loupeHelper = new DeployDiamondLoupe();
  DeployIntrospection internal introspectionHelper = new DeployIntrospection();
  DeployOwnable internal ownableHelper = new DeployOwnable();

  NodeRegistryHelper internal nodeRegistryHelper = new NodeRegistryHelper();
  StreamRegistryHelper internal streamRegistryHelper =
    new StreamRegistryHelper();
  OperatorRegistryHelper internal operatorRegistryHelper =
    new OperatorRegistryHelper();
  RiverConfigHelper internal riverConfigHelper = new RiverConfigHelper();

  // deployer
  DeployRiverConfig internal deployRiverConfig = new DeployRiverConfig();
  DeployMultiInit deployMultiInit = new DeployMultiInit();

  address internal diamondCut;
  address internal diamondLoupe;
  address internal introspection;
  address internal ownable;
  address internal nodeRegistry;
  address internal streamRegistry;
  address internal operatorRegistry;
  address internal riverConfig;

  address[] internal operators = new address[](1);
  address[] internal configManagers = new address[](1);

  function versionName() public pure override returns (string memory) {
    return "riverRegistry";
  }

  function diamondInitParams(
    address deployer
  ) public override returns (Diamond.InitParams memory) {
    diamondCut = cutHelper.deploy();
    diamondLoupe = loupeHelper.deploy();
    introspection = introspectionHelper.deploy();
    ownable = ownableHelper.deploy();
    address multiInit = deployMultiInit.deploy();
    riverConfig = deployRiverConfig.deploy();

    operators[0] = deployer;
    configManagers[0] = deployer;

    vm.startBroadcast(deployer);

    nodeRegistry = address(new NodeRegistry());
    streamRegistry = address(new StreamRegistry());
    operatorRegistry = address(new OperatorRegistry());
    vm.stopBroadcast();

    addFacet(
      ownableHelper.makeCut(ownable, IDiamond.FacetCutAction.Add),
      ownable,
      ownableHelper.makeInitData(deployer)
    );
    addFacet(
      cutHelper.makeCut(diamondCut, IDiamond.FacetCutAction.Add),
      diamondCut,
      cutHelper.makeInitData("")
    );
    addFacet(
      loupeHelper.makeCut(diamondLoupe, IDiamond.FacetCutAction.Add),
      diamondLoupe,
      loupeHelper.makeInitData("")
    );
    addFacet(
      introspectionHelper.makeCut(introspection, IDiamond.FacetCutAction.Add),
      introspection,
      introspectionHelper.makeInitData("")
    );
    addFacet(
      operatorRegistryHelper.makeCut(
        operatorRegistry,
        IDiamond.FacetCutAction.Add
      ),
      operatorRegistry,
      operatorRegistryHelper.makeInitData(operators)
    );
    addFacet(
      riverConfigHelper.makeCut(riverConfig, IDiamond.FacetCutAction.Add),
      riverConfig,
      riverConfigHelper.makeInitData(configManagers)
    );
    addCut(
      nodeRegistryHelper.makeCut(nodeRegistry, IDiamond.FacetCutAction.Add)
    );
    addCut(
      streamRegistryHelper.makeCut(streamRegistry, IDiamond.FacetCutAction.Add)
    );

    return
      Diamond.InitParams({
        baseFacets: baseFacets(),
        init: multiInit,
        initData: abi.encodeWithSelector(
          MultiInit.multiInit.selector,
          _initAddresses,
          _initDatas
        )
      });
  }
}
